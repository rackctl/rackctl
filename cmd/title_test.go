package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/doctor"
	"github.com/rackctl/rackctl/internal/exec"
)

// The banner must carry the two fields that decide WHICH CLOUD is about to change.
//
// A region is shared by every account an operator has, and an org name says nothing about
// credentials — so an operator confirming a title that shows only those two has confirmed
// nothing. The account id and the profile are the load-bearing halves.
func TestCommandTitle_NamesTheAccountAndTheProfile(t *testing.T) {
	cfg := config.Default()
	cfg.Org.Name = "acme"
	cfg.Cloud.AccountID = "477165397189"
	cfg.Cloud.Profile = "acme-production"
	cfg.Cloud.Region = "us-east-1"
	cfg.Environment = config.EnvProduction

	got := commandTitle("destroy", cfg)
	for _, want := range []string{"destroy", "acme", "477165397189", "acme-production", "us-east-1", "production"} {
		if !strings.Contains(got, want) {
			t.Errorf("the banner must carry %q — it is what an operator confirms before a "+
				"destructive run.\ngot: %s", want, got)
		}
	}
}

// Two environments of one platform must not render the same banner, or confirming it
// proves nothing.
func TestCommandTitle_DistinguishesEnvironments(t *testing.T) {
	dev, prod := config.Default(), config.Default()
	dev.Environment, prod.Environment = config.EnvDev, config.EnvProduction
	dev.Cloud.AccountID, prod.Cloud.AccountID = "111111111111", "222222222222"

	if commandTitle("destroy", dev) == commandTitle("destroy", prod) {
		t.Fatal("two different accounts and environments rendered an identical banner")
	}
}

// EKS writes the cluster ARN into the kubeconfig; the check compares a bare name.
func TestEKSClusterName_ExtractsTheNameFromAnARN(t *testing.T) {
	got := eksClusterName("arn:aws:eks:us-east-1:477165397189:cluster/production-platform")
	if got != "production-platform" {
		t.Fatalf("got %q, want %q", got, "production-platform")
	}
}

// A non-EKS context is returned unchanged so it is COMPARED — and therefore refused —
// rather than parsed into something that happens to match the configured name.
func TestEKSClusterName_LeavesANonEKSContextAlone(t *testing.T) {
	for _, in := range []string{"kind-kx", "minikube", ""} {
		if got := eksClusterName(in); got != in {
			t.Errorf("eksClusterName(%q) = %q — a non-EKS context must be reported as itself", in, got)
		}
	}
}

// A partition other than aws must still parse: govcloud and china write different ARNs.
func TestEKSClusterName_HandlesOtherPartitions(t *testing.T) {
	got := eksClusterName("arn:aws-us-gov:eks:us-gov-west-1:111111111111:cluster/production-platform")
	if got != "production-platform" {
		t.Fatalf("got %q, want %q", got, "production-platform")
	}
}

// currentKubeCluster drives the production Runner, so a kubeconfig holding an ARN must
// come back as the bare name the caller compares against cfg.ClusterName().
func TestCurrentKubeCluster_ReturnsTheBareName(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\necho 'arn:aws:eks:us-east-1:111111111111:cluster/development-platform'\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake kubectl: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	got, err := currentKubeCluster(t.Context(), exec.New(&strings.Builder{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "development-platform" {
		t.Fatalf("got %q, want %q", got, "development-platform")
	}
}

// An unreachable kubeconfig is an error, not an empty cluster name — the caller decides
// whether to run the cluster checks on the answer, and "" would compare as a mismatch
// against a configured name rather than as "could not look".
func TestCurrentKubeCluster_UnreadableIsAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write fake kubectl: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if _, err := currentKubeCluster(t.Context(), exec.New(&strings.Builder{})); err == nil {
		t.Fatal("an unreadable kubeconfig must be an error, not an empty cluster name")
	}
}

// Every doctor.Status must render through a ui helper. A status with no arm falls through
// silently, so the check that produced it disappears from the operator's output entirely.
func TestPrintResults_RendersEveryStatus(t *testing.T) {
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	printResults([]doctor.Result{
		{Name: "identity", Status: doctor.OK, Detail: "caller matches"},
		{Name: "quota", Status: doctor.Warn, Detail: "32 vCPU"},
		{Name: "collisions", Status: doctor.Fail, Detail: "bucket taken"},
		{Name: "dashboards", Status: doctor.Skip, Detail: "floor tier"},
	})
	_ = w.Close()
	os.Stdout = old

	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}

	got := b.String()
	for _, want := range []string{"identity", "quota", "collisions", "dashboards"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q never reached the operator — a status with no arm renders nothing "+
				"and the check silently disappears.\ngot: %s", want, got)
		}
	}
	if lines := strings.Count(strings.TrimSpace(got), "\n") + 1; lines != 4 {
		t.Errorf("got %d lines, want 4 — one per result", lines)
	}
}
