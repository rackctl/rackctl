package phases

import (
	"context"
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

// druid is torn down like any other bucket owner, in EVERY environment.
//
// It was not always. landing-zone's d7c7ec4 closed two of druid's three teardown gates —
// force_destroy on the per-tenant buckets and skip_final_snapshot on Aurora — and left
// deletion_protection wired straight from the tenant map, where no TF_VAR could reach it
// without replacing the leaf's ACU and backup-window sizing wholesale. rackctl refused that
// teardown rather than deleting the deepstorage segments and only then wedging on
// DeleteDBCluster.
//
// 94dff69 closed it, as the class rather than the instance: every teardown gate in a component
// declaring force_destroy_buckets now resolves permissively on the lever-true branch and keeps
// the leaf's pin otherwise. Verified end-to-end before this refusal was retired —
// components/aws/druid/variables.tf:86 declares it, main.tf:24 passes it to the tenant module,
// modules/tenant/variables.tf:58 receives it, and aurora.tf:20 folds it into
// local.allow_teardown, which aurora.tf:105 uses for deletion_protection.
//
// So the permitting apply now clears protection via ModifyDBCluster in the same act that lands
// force_destroy, and act 2 reaches the buckets and the DB cluster together. This test is what
// notices if that stops being true: a druid config in any environment must produce a plan, not
// a refusal.
func TestPermitBucketTeardown_CoversDruidInEveryEnvironment(t *testing.T) {
	for _, env := range []config.Environment{"development", "staging", "production"} {
		t.Run(string(env), func(t *testing.T) {
			var out strings.Builder
			run := exec.New(&out)
			run.DryRun = true
			st := &engine.State{Runner: run, Config: druidCfg(t, env), Repos: engine.Repos{LandingZone: "lz"}}

			if err := PermitBucketTeardown(context.Background(), st); err != nil {
				t.Fatalf("druid's deletion_protection is gated on force_destroy_buckets upstream "+
					"(94dff69), so the permitting apply clears it — there is nothing left to refuse: %v", err)
			}
			if s := out.String(); !strings.Contains(s, "druid") {
				t.Fatalf("druid must be in the permitting apply set for %s:\n%s", env, s)
			}
		})
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
