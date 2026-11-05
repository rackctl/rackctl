package cmd

import (
	"os"
	osexec "os/exec"
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
	cfg.Cloud.AccountID = "123456789012"
	cfg.Cloud.Profile = "acme-production"
	cfg.Cloud.Region = "us-east-1"
	cfg.Environment = config.EnvProduction

	got := commandTitle("destroy", cfg)
	for _, want := range []string{"destroy", "acme", "123456789012", "acme-production", "us-east-1", "production"} {
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

// The header lists every value the config left out, with the value used, and says nothing
// when the config named them all.
func TestDefaultedLines_NameEachDefaultedValue(t *testing.T) {
	cfg := config.Default()
	cfg.ApplyDefaults()
	if got := defaultedLines(cfg); got != nil {
		t.Errorf("a config that names every value must print no defaults block, got %q", got)
	}
	cfg.Cluster.EndpointPublicAccess = nil
	cfg.Quotas.VCPU = 0
	cfg.ApplyDefaults()
	got := strings.Join(defaultedLines(cfg), "\n")
	for _, want := range []string{"2 value(s) not set", "cluster.endpointPublicAccess: true", "quotas.vcpu: 256"} {
		if !strings.Contains(got, want) {
			t.Errorf("the defaults block must carry %q:\n%s", want, got)
		}
	}
}

// EKS writes the cluster ARN into the kubeconfig; the check compares a bare name.
func TestEKSClusterName_ExtractsTheNameFromAnARN(t *testing.T) {
	got := eksClusterName("arn:aws:eks:us-east-1:123456789012:cluster/production-platform")
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

// currentKubeCluster drives the production Runner, and a kubeconfig holding an ARN comes
// back whole: the account and the region in it are what the wrong-cluster guard compares.
func TestCurrentKubeCluster_ReturnsTheWholeEntry(t *testing.T) {
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
	if want := "arn:aws:eks:us-east-1:111111111111:cluster/development-platform"; got != want {
		t.Fatalf("got %q, want %q", got, want)
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

// plan prints the defaults block under its title, through the binary, so an operator reading
// the plan sees each value the config left out before reading what it would build.
func TestPlan_PrintsEveryDefaultedValueUnderTheTitle(t *testing.T) {
	bin := buildRackctl(t)
	stubs := t.TempDir()
	for _, tool := range []string{"tofu", "terragrunt", "kubectl", "helm", "git", "gh", "aws"} {
		if err := os.WriteFile(filepath.Join(stubs, tool), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := filepath.Join(t.TempDir(), "rackctl.yaml")
	if err := os.WriteFile(cfg, []byte(`org:
  name: acme
cloud:
  accountId: "111111111111"
  region: us-west-2
  profile: test
environment: development
cluster:
  name: platform
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := osexec.Command(bin, "plan", "-c", cfg)
	cmd.Env = append(os.Environ(), "PATH="+stubs, "HOME="+t.TempDir())
	out, _ := cmd.CombinedOutput()
	title := strings.Index(string(out), "rackctl plan")
	block := strings.Index(string(out), "value(s) not set in the config, using defaults")
	if title < 0 || block < title {
		t.Fatalf("the defaults block must follow the plan title:\n%s", out)
	}
	for _, want := range []string{"cluster.endpointPublicAccess: true", "agentPlatform.compliance.soc2: true",
		"cluster.systemNodes.minSize: 2"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("the plan must name the defaulted value %q:\n%s", want, out)
		}
	}
}
