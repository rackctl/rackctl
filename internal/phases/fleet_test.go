package phases

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/engine"
	"github.com/rackctl/rackctl/internal/exec"
)

// The old fleet phase applied eks-fleet/crossplane.yaml with Runner.Dir still on
// landing-zone — a path that never existed. A dry-run with eksFleet: true must now
// (a) have acquired an eks-fleet checkout path and (b) issue kubectl/helm commands
// against files that exist in that repo.
func TestFleet_DryRunUsesTheEKSFleetCheckout(t *testing.T) {
	// The fixture below is the unit test everywhere. RACKCTL_EKSFLEET_CHECKOUT opts a
	// developer (or an integration job) into running the same assertions against a real
	// clone — which is the only way this catches upstream path drift.
	//
	// It used to hardcode an absolute path under one developer's home. That made the
	// property the test advertises ("commands against paths that exist in the eks-fleet
	// checkout") verifiable on exactly one machine: in CI the fixture is built from the
	// same six path strings the phase applies, so it can only ever agree with itself. If
	// upstream renames config/reaper.yaml, CI stays green and the phase 404s at apply time.
	checkout := os.Getenv("RACKCTL_EKSFLEET_CHECKOUT")
	if _, err := os.Stat(filepath.Join(checkout, "apis/cluster/definition.yaml")); err != nil {
		checkout = t.TempDir()
		for _, p := range []string{
			"config/bootstrap/providers.yaml",
			"config/functions.yaml",
			"config/providers/providerconfig.yaml",
			"apis/cluster/definition.yaml",
			"compositions/cluster-aws.yaml",
			"config/reaper.yaml",
		} {
			dir := filepath.Join(checkout, filepath.Dir(p))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(checkout, p), []byte("# fixture\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	var out strings.Builder
	run := exec.New(&out)
	run.DryRun = true
	// Poison Dir the way the old bug did: leave it on a landing-zone path that has no
	// eks-fleet/ tree. fleet.Run must overwrite it.
	run.Dir = t.TempDir()

	cfg := config.Default()
	cfg.ControlPlane.EKSFleet = true
	cfg.Org.GitOps.ClustersRepo = "github.com/acme/clusters"
	st := &engine.State{
		Config: cfg,
		Runner: run,
		Repos:  engine.Repos{EKSFleet: checkout},
	}

	if err := (fleet{}).Run(context.Background(), st); err != nil {
		t.Fatalf("fleet.Run: %v", err)
	}

	if run.Dir != checkout {
		t.Fatalf("Runner.Dir = %q, want the eks-fleet checkout %q — otherwise every -f path resolves against the wrong tree", run.Dir, checkout)
	}

	got := out.String()
	for _, want := range []string{
		"config/bootstrap/providers.yaml",
		"config/functions.yaml",
		"config/providers/providerconfig.yaml",
		"apis/cluster/definition.yaml",
		"compositions/cluster-aws.yaml",
		"config/reaper.yaml",
		"crossplane-stable/crossplane",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("dry-run must print a command against %s (a path that exists in eks-fleet); got:\n%s", want, got)
		}
	}
	// The Configuration package meta is NOT the install surface.
	if strings.Contains(got, "crossplane.yaml") {
		t.Errorf("must not apply crossplane.yaml — that is package meta, not an installable set of resources.\ngot:\n%s", got)
	}
	// And the old relative path under landing-zone must be gone.
	if strings.Contains(got, "eks-fleet/crossplane.yaml") {
		t.Errorf("must not apply eks-fleet/crossplane.yaml relative to landing-zone.\ngot:\n%s", got)
	}

	// Every -f path must resolve under the checkout.
	for _, rel := range []string{
		"config/bootstrap/providers.yaml",
		"config/functions.yaml",
		"config/providers/providerconfig.yaml",
		"apis/cluster/definition.yaml",
		"compositions/cluster-aws.yaml",
		"config/reaper.yaml",
	} {
		if _, err := os.Stat(filepath.Join(checkout, rel)); err != nil {
			t.Errorf("%s does not exist under the checkout used as Runner.Dir: %v", rel, err)
		}
	}
}

// acquire must clone nanohype/eks-fleet when the gate is on, and leave it alone otherwise.
func TestAcquire_ClonesEKSFleetWhenGated(t *testing.T) {
	var out strings.Builder
	run := exec.New(&out)
	run.DryRun = true

	cfg := config.Default()
	cfg.Org.Name = "acme"
	cfg.ControlPlane.EKSFleet = true
	cfg.Org.GitOps.ClustersRepo = "github.com/acme/clusters"
	// Skip the live-root guard: dry-run without a real landing-zone tree would fail it.
	// We only care that the clone line is printed, so assertComponentRoots needs a tree.
	// Build a bare landing-zone with the default core components' roots.
	lz := t.TempDir()
	// Minimal .git so the guard treats it as a checkout, plus every CoreComponents root.
	if err := os.MkdirAll(filepath.Join(lz, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg.Cloud.Region = "us-west-2"
	cfg.Environment = config.EnvDev
	// agent platform on + full obs → agent-iam, managed-monitoring, observability, cluster-addons, cluster-bootstrap
	for _, c := range []string{"network", "cluster", "secrets", "agent-iam", "managed-monitoring", "observability", "cluster-addons", "cluster-bootstrap"} {
		dir := filepath.Join(lz, "live", "aws", "workload-development", "us-west-2", "development", c)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "terragrunt.hcl"), []byte("inputs = {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Point RepoPaths at our temp by overriding after Run starts — actually acquire
	// calls RepoPaths itself. Patch by running with HOME set so ~/.rackctl/acme lands
	// in a temp dir.
	home := t.TempDir()
	t.Setenv("HOME", home)

	st := &engine.State{Config: cfg, Runner: run}
	// Inject the landing-zone after acquire would set it — we need to intercept.
	// Simpler: call clone logic indirectly by checking printed lines, and place the
	// landing-zone where RepoPaths puts it so assertComponentRoots finds it.
	repos := engine.RepoPaths("acme")
	// Symlink our fixture tree into the expected path.
	if err := os.MkdirAll(filepath.Dir(repos.LandingZone), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(lz, repos.LandingZone); err != nil {
		t.Fatal(err)
	}

	if err := (acquire{}).Run(context.Background(), st); err != nil {
		t.Fatalf("acquire.Run: %v\nout:\n%s", err, out.String())
	}

	got := out.String()
	if !strings.Contains(got, "https://github.com/nanohype/eks-fleet.git") {
		t.Fatalf("acquire must clone nanohype/eks-fleet when eksFleet is on.\ngot:\n%s", got)
	}
	if st.Repos.EKSFleet == "" || !strings.HasSuffix(st.Repos.EKSFleet, "eks-fleet") {
		t.Fatalf("Repos.EKSFleet must be set to the eks-fleet path; got %q", st.Repos.EKSFleet)
	}

	// Off by default: no clone.
	out.Reset()
	cfg.ControlPlane.EKSFleet = false
	cfg.Org.GitOps.ClustersRepo = ""
	run2 := exec.New(&out)
	run2.DryRun = true
	st2 := &engine.State{Config: cfg, Runner: run2}
	// Landing-zone still needed for the root guard.
	if err := os.Remove(repos.LandingZone); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(lz, repos.LandingZone); err != nil {
		t.Fatal(err)
	}
	if err := (acquire{}).Run(context.Background(), st2); err != nil {
		t.Fatalf("acquire.Run (fleet off): %v", err)
	}
	if strings.Contains(out.String(), "eks-fleet") {
		t.Fatalf("acquire must not clone eks-fleet when the gate is off.\ngot:\n%s", out.String())
	}
}

// fleet.Run refuses to invent a Dir when acquire never ran — better a clear error than
// applying relative paths into whatever the previous phase left as cwd.
func TestFleet_RequiresTheCheckout(t *testing.T) {
	run := exec.New(io.Discard)
	run.DryRun = true
	cfg := config.Default()
	cfg.ControlPlane.EKSFleet = true
	cfg.Org.GitOps.ClustersRepo = "github.com/acme/clusters"
	st := &engine.State{Config: cfg, Runner: run} // Repos.EKSFleet empty

	err := (fleet{}).Run(context.Background(), st)
	if err == nil || !strings.Contains(err.Error(), "Repos.EKSFleet") {
		t.Fatalf("want an error naming Repos.EKSFleet, got %v", err)
	}
}
