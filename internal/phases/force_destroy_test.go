package phases

import (
	"context"
	"io"
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
