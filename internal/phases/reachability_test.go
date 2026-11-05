package phases

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/engine"
	"github.com/rackctl/rackctl/internal/exec"
)

// A cluster whose API does not answer from this host fails the phase there, naming the
// endpoint posture — rather than in cluster-bootstrap as a provider dial error, after the
// substrate is built. The cloud is correct, so it is left standing.
func TestClusterPhase_AnUnreachableAPIIsLeftStanding(t *testing.T) {
	st := fakeAWSAndTG(t, 0)
	dir := t.TempDir()
	probe := filepath.Join(dir, "argv")
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte("#!/bin/sh\necho \"$*\" > "+probe+
		"\necho 'Unable to connect to the server: dial tcp 10.0.1.5:443: i/o timeout' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	st.Runner.ReadAttempts = 1

	err := (cluster{}).Run(context.Background(), st)
	var nr *engine.NoRollbackError
	if !errors.As(err, &nr) {
		t.Fatalf("an unreachable API must leave the built cluster standing, got %v", err)
	}
	for _, want := range []string{"i/o timeout", "PRIVATE", "development-platform"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must carry %q: %v", want, err)
		}
	}
	argv, _ := os.ReadFile(probe)
	if !strings.Contains(string(argv), "get --raw /readyz --request-timeout=") {
		t.Errorf("the probe must be the bounded readyz read, got %q", argv)
	}
	// The kubeconfig WAS repointed, so the fact stands.
	if st.KubeconfigCluster != st.Config.ClusterName() {
		t.Errorf("KubeconfigCluster = %q after a successful repoint", st.KubeconfigCluster)
	}
}

// A dry-run probes nothing.
func TestClusterPhase_DryRunDoesNotProbe(t *testing.T) {
	st := fakeAWSAndTG(t, 0)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	st.Runner = exec.New(&strings.Builder{})
	st.Runner.DryRun = true
	if err := (cluster{}).Run(context.Background(), st); err != nil {
		t.Fatalf("a dry-run must not probe an API it did not build: %v", err)
	}
}

func TestEndpointPosture_NamesWhoTheEndpointAdmits(t *testing.T) {
	for _, tc := range []struct {
		c    config.Cluster
		want string
	}{
		{config.Cluster{}, "PRIVATE"},
		{config.Cluster{EndpointPublicAccess: true, EndpointAllowlist: []string{"203.0.113.0/24"}}, "203.0.113.0/24"},
		{config.Cluster{EndpointPublicAccess: true}, "egress IP this host had"},
	} {
		if got := endpointPosture(tc.c); !strings.Contains(got, tc.want) {
			t.Errorf("endpointPosture(%+v) = %q, want it to carry %q", tc.c, got, tc.want)
		}
	}
}

// An expired convergence wait says which state it ended in, because the three want
// different next moves.
func TestCatalogVerdict_ClassifiesWhatTheWaitSaw(t *testing.T) {
	denied := errors.New("kubectl: Forbidden")
	for _, tc := range []struct {
		name      string
		everRead  bool
		count     int
		unhealthy []string
		lastErr   error
		want      []string
	}{
		{"never readable", false, 0, nil, denied, []string{"never readable", "Forbidden"}},
		{"no children", true, 0, nil, nil, []string{"generated no child Applications"}},
		{"named unhealthy", true, 44, []string{"opencost (Degraded/Synced)", "loki (Progressing/Synced)"}, nil,
			[]string{"2 of 44", "opencost (Degraded/Synced)", "loki"}},
		{"last read failed", true, 44, []string{"opencost (Degraded/Synced)"}, denied,
			[]string{"1 of 44", "final read failed", "Forbidden"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := catalogVerdict(30*time.Minute, tc.everRead, tc.count, tc.unhealthy, tc.lastErr)
			for _, w := range tc.want {
				if err == nil || !strings.Contains(err.Error(), w) {
					t.Errorf("want %q in %v", w, err)
				}
			}
			if tc.lastErr != nil && !errors.Is(err, tc.lastErr) {
				t.Errorf("the read's error must be wrapped, not flattened: %v", err)
			}
		})
	}
}

// The convergence failure carries the wait's classified error with %w, rather than a fixed
// sentence that dropped what the wait saw.
func TestGitopsPhase_ConvergenceFailureWrapsWhatTheWaitSaw(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"terragrunt": "#!/bin/sh\nexit 0\n",
		"kubectl":    "#!/bin/sh\necho 'Error from server (Forbidden)' >&2\nexit 1\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := config.Default()
	cfg.Org.Name = "acme"
	cfg.Cluster.Name = "platform"
	cfg.ApplyDefaults()
	st := &engine.State{
		Config: cfg,
		Runner: exec.New(&strings.Builder{}),
		Repos:  engine.Repos{Workdir: dir, LandingZone: dir},
	}
	st.Runner.ReadAttempts = 1

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	err := gitopsPhase{}.Run(ctx, st)
	var nr *engine.NoRollbackError
	if !errors.As(err, &nr) {
		t.Fatalf("a convergence failure must leave the cloud standing, got %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the wait's own error must be wrapped with %%w: %v", err)
	}
}

// Helm finds a release only in the namespace it is told. The portal is installed into
// tenants-portal, so an uninstall with no namespace looked in `default`, found nothing, and
// reported success over a release still running.
func TestPortalTeardown_UninstallsFromTheNamespaceItInstalledInto(t *testing.T) {
	dir := t.TempDir()
	argv := filepath.Join(dir, "argv")
	if err := os.WriteFile(filepath.Join(dir, "helm"), []byte("#!/bin/sh\necho \"$*\" > "+argv+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	st := &engine.State{Config: baseCfg(), Runner: exec.New(&strings.Builder{})}
	if err := (portal{}).Teardown(context.Background(), st); err != nil {
		t.Fatalf("portal teardown: %v", err)
	}
	got, _ := os.ReadFile(argv)
	if !strings.Contains(string(got), "uninstall portal -n "+portalNamespace) {
		t.Errorf("helm uninstall must name the release's namespace, got %q", got)
	}
}

// The cluster teardown's volume sweep reports itself on State, so the rollback's summary
// covers it alongside the sweeps the engine runs.
func TestClusterTeardown_RecordsItsVolumeBackstops(t *testing.T) {
	st := fakeAWSAndTG(t, 0)
	if err := (cluster{}).Teardown(context.Background(), st); err != nil {
		t.Fatalf("cluster teardown: %v", err)
	}
	var names []string
	for _, b := range st.Backstops {
		names = append(names, b.Name+"="+b.Outcome.String())
	}
	got := strings.Join(names, ", ")
	if !strings.Contains(got, "unprovable EBS volumes=found nothing") || !strings.Contains(got, "orphaned EBS volumes=found nothing") {
		t.Errorf("the volume sweep's backstops must be on State, got %q", got)
	}
}
