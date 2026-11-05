package cmd

import (
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
	bin := buildRackctl(t)
	stubs := t.TempDir()
	for _, tool := range []string{"tofu", "terragrunt", "kubectl", "helm", "git", "gh"} {
		if err := os.WriteFile(filepath.Join(stubs, tool), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(stubs, "aws"), []byte("#!/bin/sh\n"+awsBody), 0o755); err != nil {
		t.Fatal(err)
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
agentPlatform:
  enable: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "destroy", "-c", cfg, "--dry-run")
	cmd.Env = append(os.Environ(), "PATH="+stubs, "HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	return string(out), err
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
	// Every aws read is denied: the IAM and EC2 backstops cannot enumerate.
	out, err := runDestroyDryRun(t, "echo 'An error occurred (AccessDenied)' >&2\nexit 254\n")
	if err != nil {
		t.Fatalf("the exit status is unchanged here: %v\n%s", err, out)
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
