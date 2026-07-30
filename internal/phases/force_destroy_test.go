package phases

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/engine"
	"github.com/rackctl/rackctl/internal/exec"
)

// A default config (agent platform on, no druid/model-import) permits exactly
// agent-iam and cluster-addons — the two that always own buckets on the core path.
func TestForceDestroyBucketComponents_DefaultConfig(t *testing.T) {
	cfg := config.Default()
	cfg.Org.Name = "acme"
	cfg.Cloud.AccountID = "111111111111"
	cfg.Cloud.Profile = "p"
	cfg.Cluster.Name = "platform"
	cfg.ApplyDefaults()

	got := ForceDestroyBucketComponents(cfg)
	want := []string{"agent-iam", "cluster-addons"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// Gating model-import and druid on must add them to the permitting set, in CoreComponents order.
func TestForceDestroyBucketComponents_GatedOn(t *testing.T) {
	cfg := config.Default()
	cfg.Org.Name = "acme"
	cfg.Cloud.AccountID = "111111111111"
	cfg.Cloud.Profile = "p"
	cfg.Cloud.Region = "us-west-2"
	cfg.Cluster.Name = "platform"
	cfg.AgentPlatform.ModelImport = true
	cfg.Addons.Druid = true
	cfg.ApplyDefaults()

	got := ForceDestroyBucketComponents(cfg)
	// CoreComponents order: agent-iam, …, druid, …, model-import, cluster-addons
	// (model-import is after dns and before cluster-addons; druid is after observability).
	seen := map[string]bool{}
	for _, c := range got {
		seen[c] = true
	}
	for _, want := range []string{"agent-iam", "cluster-addons", "model-import", "druid"} {
		if !seen[want] {
			t.Errorf("missing %s in %v", want, got)
		}
	}
}

// PermitBucketTeardown must print both acts and apply each component with the flag.
// A dry-run records the commands; the flag must appear on the apply, not only as prose.
func TestPermitBucketTeardown_TwoActs(t *testing.T) {
	var out strings.Builder
	run := exec.New(&out)
	run.DryRun = true

	cfg := config.Default()
	cfg.Org.Name = "acme"
	cfg.Cloud.AccountID = "111111111111"
	cfg.Cloud.Profile = "p"
	cfg.Cluster.Name = "platform"
	cfg.Environment = config.EnvStaging // louder note outside development
	cfg.ApplyDefaults()

	st := &engine.State{
		Config: cfg,
		Runner: run,
		Repos:  engine.Repos{LandingZone: t.TempDir()},
	}

	if err := PermitBucketTeardown(context.Background(), st); err != nil {
		t.Fatalf("PermitBucketTeardown: %v", err)
	}

	got := out.String()
	for _, want := range []string{
		"two acts",
		"TF_VAR_force_destroy_buckets=true",
		"velero_backup_policy",
		"agent-iam",
		"cluster-addons",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("dry-run must mention %q.\ngot:\n%s", want, got)
		}
	}
	// The flag must ride the apply command, not only a note.
	if !strings.Contains(got, "apply") {
		t.Errorf("must issue apply commands; got:\n%s", got)
	}
}

// Platform off + no druid/model-import → nothing to permit.
func TestPermitBucketTeardown_NothingToDo(t *testing.T) {
	run := exec.New(io.Discard)
	run.DryRun = true
	off := false
	cfg := config.Default()
	cfg.AgentPlatform.Enable = &off
	cfg.ApplyDefaults()

	st := &engine.State{Config: cfg, Runner: run, Repos: engine.Repos{LandingZone: t.TempDir()}}
	if err := PermitBucketTeardown(context.Background(), st); err != nil {
		t.Fatalf("empty set must be a no-op: %v", err)
	}
	// cluster-addons is still in CoreComponents even with platform off.
	if got := ForceDestroyBucketComponents(cfg); len(got) != 1 || got[0] != "cluster-addons" {
		// platform off still applies cluster-addons
		if len(got) == 0 {
			t.Fatal("cluster-addons should still be permitted when the platform is off")
		}
	}
}

// druidCfg builds a config with druid gated on, in the given environment.
func druidCfg(t *testing.T, env config.Environment) *config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Org.Name = "acme"
	cfg.Cloud.AccountID = "111111111111"
	cfg.Cloud.Profile = "p"
	cfg.Cloud.Region = "us-west-2"
	cfg.Cluster.Name = "platform"
	cfg.Environment = env
	cfg.Addons.Druid = true
	cfg.ApplyDefaults()
	return cfg
}

// --force-buckets must REFUSE a druid teardown outside development rather than run act 1.
//
// landing-zone's d7c7ec4 closed two of druid's three teardown gates — force_destroy on the
// per-tenant buckets and skip_final_snapshot on Aurora, both wired to
// `local.allow_teardown`. It left `deletion_protection = var.tenant_config.deletion_protection`
// (modules/tenant/aurora.tf:94), whose component default is true and which BOTH non-development
// leaves pin true. The permitting apply cannot clear it: it lives inside a map(object), so the
// only ambient channel is a wholesale TF_VAR_tenants that would also replace the leaf's ACU
// and backup-window sizing.
//
// Continuing anyway is worse than stopping. Act 2 deletes druid's deepstorage, indexlogs and
// msq buckets — deepstorage has neither versioning nor expiry, so a tenant's Druid segments
// have no other copy — and only then fails on DeleteDBCluster, halting the sweep before
// cluster and network with the EKS cluster, VPC and NAT gateways still billing.
func TestPermitBucketTeardown_RefusesDruidOutsideDevelopment(t *testing.T) {
	for _, env := range []config.Environment{"staging", "production"} {
		t.Run(string(env), func(t *testing.T) {
			var out strings.Builder
			run := exec.New(&out)
			run.DryRun = true
			st := &engine.State{Runner: run, Config: druidCfg(t, env), Repos: engine.Repos{LandingZone: "lz"}}

			err := PermitBucketTeardown(context.Background(), st)
			if err == nil {
				t.Fatal("expected a refusal — act 1 cannot clear Aurora deletion_protection, and act 2 " +
					"would delete the deepstorage segments before wedging")
			}
			// It must be a NoRollbackError: refusing to start a teardown is a precondition
			// failure, and must never itself trigger a destructive sweep.
			var norb *engine.NoRollbackError
			if !errors.As(err, &norb) {
				t.Fatalf("refusal must be a *engine.NoRollbackError, got %T", err)
			}
			// The operator has to learn WHICH gate stopped them, or they cannot clear it.
			if !strings.Contains(err.Error(), "deletion_protection") {
				t.Fatalf("the refusal must name deletion_protection as the gate:\n%s", err)
			}
			// And nothing may have been run OR announced: the refusal comes before the
			// two-act notes, so the operator is never told about a plan that will not happen.
			if s := out.String(); strings.Contains(s, "terragrunt") || strings.Contains(s, "force-buckets:") {
				t.Fatalf("act 1 must neither run nor be announced before the refusal:\n%s", s)
			}
		})
	}
}

// Development is the case that genuinely works: allow_teardown is true on environment alone,
// so the permitting apply proceeds and druid is applied like any other bucket owner.
func TestPermitBucketTeardown_AllowsDruidInDevelopment(t *testing.T) {
	var out strings.Builder
	run := exec.New(&out)
	run.DryRun = true
	st := &engine.State{Runner: run, Config: druidCfg(t, "development"), Repos: engine.Repos{LandingZone: "lz"}}

	if err := PermitBucketTeardown(context.Background(), st); err != nil {
		t.Fatalf("development must not be refused — landing-zone's allow_teardown is true on "+
			"environment alone there: %v", err)
	}
	if !strings.Contains(out.String(), "druid") {
		t.Fatalf("druid must be in the permitting apply in development:\n%s", out.String())
	}
}

// The flag must reach the terragrunt PROCESS, not just the log.
//
// The previous version of this test asserted
// strings.Contains(out, "TF_VAR_force_destroy_buckets=true") against the dry-run
// transcript. exec.Runner echoes argv and never env, so that assertion was satisfied
// entirely by PermitBucketTeardown's own notes: deleting `extraEnv` from applyWith
// altogether left every force-buckets test green, leaving the one behaviour target 11
// exists for completely unguarded. That is the same "string whitelist stayed green when
// the guarded call moved" class an earlier pass on this branch found five of.
//
// So this runs for real against a fake terragrunt on $PATH that records its own
// environment — the only way to observe what the child process was actually handed.
func TestPermitBucketTeardown_FlagReachesTheTerragruntProcess(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "invocations")
	// Record verb + whether the variable was exported, once per invocation.
	tgScript := fmt.Sprintf(`#!/bin/sh
for a in "$@"; do
  case "$a" in apply|destroy|init) verb="$a";; esac
done
echo "$verb force_destroy_buckets=${TF_VAR_force_destroy_buckets:-UNSET}" >> %q
exit 0
`, log)
	if err := os.WriteFile(filepath.Join(dir, "terragrunt"), []byte(tgScript), 0o755); err != nil {
		t.Fatalf("write fake terragrunt: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := config.Default()
	cfg.Org.Name = "acme"
	cfg.Cloud.AccountID = "111111111111"
	cfg.Cloud.Profile = "p"
	cfg.Cloud.Region = "us-west-2"
	cfg.Cluster.Name = "platform"
	cfg.ApplyDefaults()

	st := &engine.State{
		Config:  cfg,
		Runner:  exec.New(io.Discard), // NOT dry-run — the point is to exec
		Repos:   engine.Repos{Workdir: dir, LandingZone: dir},
		Outputs: map[string]string{},
	}
	if err := PermitBucketTeardown(context.Background(), st); err != nil {
		t.Fatalf("permitting apply: %v", err)
	}

	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("no terragrunt was invoked at all — the permitting apply must issue real "+
			"commands, not only print notes: %v", err)
	}
	got := string(b)

	// Every apply must carry the flag. An apply without it lands nothing in state, and the
	// destroy that follows fails on BucketNotEmpty — the exact failure two acts exist to avoid.
	var applies int
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		if !strings.HasPrefix(line, "apply ") {
			continue
		}
		applies++
		if !strings.Contains(line, "force_destroy_buckets=true") {
			t.Fatalf("a permitting apply ran WITHOUT TF_VAR_force_destroy_buckets=true:\n%s", got)
		}
	}
	if want := len(ForceDestroyBucketComponents(cfg)); applies != want {
		t.Fatalf("got %d permitting applies, want %d (one per bucket-owning component):\n%s",
			applies, want, got)
	}

	// And the flag must NOT leak past its invocation — tg() restores Runner.Env with a
	// defer, and a leak would set force_destroy on components that never asked for it.
	if len(st.Runner.Env) != 0 {
		t.Fatalf("Runner.Env must be restored after the permitting applies, got %v", st.Runner.Env)
	}
}
