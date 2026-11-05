package cmd

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The end of a destroy says which backstops held, and "platform destroyed" is printed only
// when every one that guards something ran clean. Driven through the binary in dry-run, with
// every tool stubbed, so the sweeps' own enumerations decide the summary.
func runDestroyDryRun(t *testing.T, awsBody string) (string, error) {
	t.Helper()
	return runDestroy(t, awsBody, "--dry-run")
}

// runDestroy drives the binary against stubbed tools. Without --dry-run it needs --yes and a
// landing-zone checkout to run terragrunt in; the stubs answer every component with success.
func runDestroy(t *testing.T, awsBody string, flags ...string) (string, error) {
	t.Helper()
	out, err := destroyCommand(t, map[string]string{"aws": awsBody}, flags...).CombinedOutput()
	return string(out), err
}

// destroyCommand builds the destroy invocation without running it. Each tool's stub body is
// taken from bodies when present, and answers success otherwise.
func destroyCommand(t *testing.T, bodies map[string]string, flags ...string) *exec.Cmd {
	t.Helper()
	bin := buildRackctl(t)
	stubs := t.TempDir()
	for _, tool := range []string{"tofu", "terragrunt", "kubectl", "helm", "git", "gh", "aws"} {
		body, ok := bodies[tool]
		if !ok {
			body = "exit 0\n"
		}
		if err := os.WriteFile(filepath.Join(stubs, tool), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := filepath.Join(t.TempDir(), "rackctl.yaml")
	if err := os.WriteFile(cfg, []byte(`org:
  name: acme
  gitops:
    eksGitopsRepo: github.com/acme/eks-gitops
cloud:
  provider: aws
  accountId: "111111111111"
  region: us-west-2
  profile: test
environment: development
cluster:
  name: platform
  endpointPublicAccess: false
agentPlatform:
  enable: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".rackctl", "acme", "landing-zone"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, append([]string{"destroy", "-c", cfg}, flags...)...)
	cmd.Env = append(os.Environ(), "PATH="+stubs, "HOME="+home)
	return cmd
}

func TestDestroy_PlatformDestroyedOnlyWhenTheBackstopsHeld(t *testing.T) {
	// aws answers every enumeration with nothing: each sweep looked and found nothing.
	out, err := runDestroyDryRun(t, "exit 0\n")
	if err != nil {
		t.Fatalf("a clean dry-run must exit 0: %v\n%s", err, out)
	}
	for _, want := range []string{"backstop summary", "operator IAM roles: found nothing",
		"Karpenter nodes: found nothing", "orphaned EBS volumes: found nothing", "platform destroyed"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestDestroy_ABackstopThatDidNotRunWithholdsPlatformDestroyed(t *testing.T) {
	// Every aws read but the state reads is denied: the dry-run cannot write its temporary
	// kubeconfig, so the in-cluster preview does not run, and the IAM and EC2 backstops cannot
	// enumerate. The state buckets answer empty, so every component is accounted for and the
	// backstops alone decide the status.
	out, err := runDestroyDryRun(t, "case \"$1\" in s3|s3api) exit 0 ;; esac\n"+
		"echo 'An error occurred (AccessDenied)' >&2\nexit 254\n")
	if code := exitStatus(err); code != ExitStranded {
		t.Fatalf("a guard that did not run leaves resources that may be billing: want exit %d, got %d (%v)\n%s",
			ExitStranded, code, err, out)
	}
	if !strings.Contains(out, "resources-may-remain (exit 7)") ||
		!strings.Contains(out, "these backstops did not hold: in-cluster reap, operator IAM roles, Karpenter nodes, orphaned EBS volumes") {
		t.Errorf("the error must name the status and every guard that did not hold:\n%s", out)
	}
	if strings.Contains(out, "platform destroyed") {
		t.Errorf("\"platform destroyed\" was printed over backstops that did not run:\n%s", out)
	}
	for _, want := range []string{"operator IAM roles: did NOT run", "Karpenter nodes: did NOT run",
		"orphaned EBS volumes: did NOT run", "not every backstop ran clean"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

// exitStatus is the process's exit status, or -1 when it did not exit on its own.
func exitStatus(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// The informational unprovable-volume report never decides the status: a destroy whose every
// guard held exits 0 even when that report could not read.
func TestDestroy_AnInformationalReportThatDidNotRunKeepsExitZero(t *testing.T) {
	out, err := runDestroyDryRun(t, `case "$*" in
  *Tags:Tags*) echo 'An error occurred (AccessDenied)' >&2; exit 254 ;;
esac
exit 0
`)
	if code := exitStatus(err); code != 0 {
		t.Fatalf("an informational report decides nothing: want exit 0, got %d\n%s", code, out)
	}
	if !strings.Contains(out, "unprovable EBS volumes: did NOT run") || !strings.Contains(out, "platform destroyed") {
		t.Errorf("the report still prints, and the guards that held still say so:\n%s", out)
	}
}

// The idempotent re-run after a teardown that already deleted the cluster: update-kubeconfig
// fails because there is no cluster, and there is nothing in it to reap. That is a clean
// teardown, not a reap that did not run.
func TestDestroy_AClusterAlreadyGoneIsACleanTeardown(t *testing.T) {
	notFound := "echo 'An error occurred (ResourceNotFoundException) when calling the DescribeCluster operation: No cluster found for name: development-platform.' >&2; exit 254"
	out, err := runDestroy(t, `case "$1 $2" in
  "eks update-kubeconfig") `+notFound+` ;;
  "eks describe-cluster")  `+notFound+` ;;
esac
exit 0
`, "--yes")
	if code := exitStatus(err); code != 0 {
		t.Fatalf("a destroy of a cluster that is already gone must exit 0, got %d\n%s", code, out)
	}
	for _, want := range []string{"in-cluster reap: found nothing — the cluster no longer exists",
		"unstick Terminating CRs: found nothing", "platform destroyed"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

// A repoint that failed for any other reason is a reap that did not run, and says so in the
// status; the unstick line that rides on the same repoint does not decide it a second time.
func TestDestroy_ARepointThatFailedForAnotherReasonIsNotClean(t *testing.T) {
	out, err := runDestroy(t, `case "$1 $2" in
  "eks update-kubeconfig") echo 'An error occurred (AccessDenied)' >&2; exit 254 ;;
  "eks describe-cluster")  echo 'An error occurred (AccessDenied)' >&2; exit 254 ;;
esac
exit 0
`, "--yes")
	if code := exitStatus(err); code != ExitStranded {
		t.Fatalf("want exit %d, got %d\n%s", ExitStranded, code, out)
	}
	if strings.Contains(out, "platform destroyed") {
		t.Errorf("\"platform destroyed\" over an in-cluster reap that did not run:\n%s", out)
	}
	if !strings.Contains(out, "these backstops did not hold: in-cluster reap —") {
		t.Errorf("only the in-cluster reap decides the status here:\n%s", out)
	}
}
