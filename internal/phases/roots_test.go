package phases

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/engine"
	"github.com/rackctl/rackctl/internal/exec"
)

// landingZone builds a fake landing-zone checkout containing a live root for each named
// component, and returns a State pointing at it. No fake binaries are needed — the guard
// only reads the filesystem.
func landingZone(t *testing.T, cfg *config.Config, present ...string) *engine.State {
	t.Helper()
	root := t.TempDir()
	st := &engine.State{
		Config: cfg,
		Runner: exec.New(io.Discard),
		Repos:  engine.Repos{LandingZone: root},
	}
	// The guard detects a checkout by .git, exactly as cloneOrUpdate does — a bare
	// directory is not a clone.
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	for _, c := range present {
		dir := filepath.Join(root, componentDir(st, c))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "terragrunt.hcl"), []byte("# leaf\n"), 0o644); err != nil {
			t.Fatalf("write leaf: %v", err)
		}
	}
	return st
}

func TestAssertComponentRoots_AllPresentPasses(t *testing.T) {
	cfg := baseCfg()
	cfg.Observability.Tier = config.TierFull
	cfg.DNS = &config.DNS{HostedZone: "example.com"}
	cfg.AgentPlatform.ModelImport = true

	st := landingZone(t, cfg, CoreComponents(cfg)...)
	if err := assertComponentRoots(st); err != nil {
		t.Fatalf("every live root present, but the guard failed: %v", err)
	}
}

// The test that would have caught the live bug.
//
// agent-iam is appended to CoreComponents BY DEFAULT (AgentPlatform.Enable nil means
// enabled) but landing-zone carries a live root for it only under workload-development.
// Without this guard a `rackctl init --apply` with environment: staging builds a VPC and
// an EKS control plane in the cluster phase, then hits a path that does not exist in the
// substrate phase — and clean-on-failure destroys both. The guard turns that into a
// one-second error, one phase after the clone and before anything is provisioned.
//
// This is the one place in the repo where asserting on error TEXT is right: the remedy is
// the operator-facing payload, and a guard that fires without naming the knob to turn off
// leaves them nowhere to go.
func TestAssertComponentRoots_MissingRootIsCaughtBeforeSpending(t *testing.T) {
	cfg := baseCfg()
	cfg.Environment = config.EnvStaging

	var present []string
	for _, c := range CoreComponents(cfg) {
		if c != "agent-iam" {
			present = append(present, c)
		}
	}
	st := landingZone(t, cfg, present...)

	err := assertComponentRoots(st)
	if err == nil {
		t.Fatal("a missing agent-iam live root must fail here — otherwise it fails in the substrate " +
			"phase, after the cluster phase has built a VPC and an EKS control plane that " +
			"clean-on-failure then destroys")
	}
	for _, want := range []string{"agent-iam", "staging", "agentPlatform.enable: false"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must name %q so the operator knows what failed and what to turn off; got: %v", want, err)
		}
	}
}

// A dry-run's verdict can only be advisory: acquire PRINTS the clone and the pull rather
// than running them, so the tree on disk is either absent or arbitrarily old. Failing a
// plan-only command over a stale checkout would block a config that applies perfectly
// once the pull actually happens, so a dry-run reports and continues. The --apply, which
// pulls first, is where it binds.
func TestAssertComponentRoots_DryRunNeverFails(t *testing.T) {
	cfg := baseCfg()
	cfg.Environment = config.EnvStaging

	run := exec.New(io.Discard)
	run.DryRun = true

	// No checkout at all — the first-ever dry-run.
	absent := &engine.State{Config: cfg, Runner: run,
		Repos: engine.Repos{LandingZone: filepath.Join(t.TempDir(), "never-cloned")}}
	if err := assertComponentRoots(absent); err != nil {
		t.Fatalf("a dry-run with no checkout must plan, not fail: %v", err)
	}

	// A checkout that exists but is missing every root — a stale clone. Still a plan.
	stale := landingZone(t, cfg) // .git present, no component leaves
	stale.Runner.DryRun = true
	if err := assertComponentRoots(stale); err != nil {
		t.Fatalf("a dry-run against a stale checkout must plan, not fail — the run never pulled: %v", err)
	}
}

// A directory that exists but is not a clone is NOT a checkout.
//
// The guard must answer "is there a checkout?" the same way cloneOrUpdate does — by
// .git, not by the directory. Getting this wrong made an interrupted clone (or a
// hand-made directory) fail on `network`, which every environment carries, with a
// remedy that could not possibly help.
func TestAssertComponentRoots_BareDirectoryIsNotACheckout(t *testing.T) {
	cfg := baseCfg()
	bare := t.TempDir() // exists, but no .git
	st := &engine.State{Config: cfg, Runner: exec.New(io.Discard), Repos: engine.Repos{LandingZone: bare}}

	if err := assertComponentRoots(st); err != nil {
		t.Fatalf("a directory with no .git is not a checkout and must not be judged: %v", err)
	}
}

// The guard must NEVER trigger the engine's rollback.
//
// Its whole premise is that nothing has been provisioned — but the engine's teardown is
// not a no-op on an empty run. It force-deletes every IAM role under /eks-agent-platform/
// ACCOUNT-wide and runs `kubectl delete platforms|tenants|nodeclaims|pvc --all -A`
// against whatever cluster the kubeconfig currently points at. An operator bootstrapping
// staging from a laptop still pointed at a healthy development cluster would have this
// guard fire correctly and then destroy the platform they were not touching.
//
// engine.Run only skips the teardown for a *engine.NoRollbackError, so that wrapper is
// the entire protection and this test is what holds it in place.
func TestAssertComponentRoots_FailureMustNotRollBack(t *testing.T) {
	cfg := baseCfg()
	cfg.Environment = config.EnvStaging

	var present []string
	for _, c := range CoreComponents(cfg) {
		if c != "agent-iam" {
			present = append(present, c)
		}
	}
	err := assertComponentRoots(landingZone(t, cfg, present...))
	if err == nil {
		t.Fatal("expected the guard to fail on the missing agent-iam root")
	}
	var noRollback *engine.NoRollbackError
	if !errors.As(err, &noRollback) {
		t.Fatalf("the guard's error MUST be a *engine.NoRollbackError — otherwise a precondition "+
			"failure sweeps account-wide IAM roles and kubectl-deletes every Platform on whatever "+
			"cluster the kubeconfig points at, none of which this run created; got %T", err)
	}
}

// A region landing-zone has no live tree for is a different diagnosis from a component
// with no leaf, and the per-component remedy cannot fix it — componentDir embeds the
// region as well as the environment, so cloud.region is the knob. This matters because
// the modelImport region allow-list blesses regions (us-east-1, us-east-2, eu-central-1)
// that landing-zone's live tree does not currently carry at all.
func TestAssertComponentRoots_MissingRegionTreeBlamesTheRegion(t *testing.T) {
	cfg := baseCfg()
	cfg.Cloud.Region = "eu-central-1"

	st := landingZone(t, cfg) // .git present, no leaves anywhere
	err := assertComponentRoots(st)
	if err == nil {
		t.Fatal("expected a failure when the region carries no live tree at all")
	}
	for _, want := range []string{"eu-central-1", "cloud.region"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("a whole-region miss must name %q — the environment remedy cannot fix it; got: %v", want, err)
		}
	}
}
