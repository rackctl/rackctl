package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/config"
)

// checkStubs puts every tool `check` requires on PATH. aws answers the identity read with
// the example's account, reports every bucket name free, and answers list-clusters with
// clusters; kubectl answers the kubeconfig read with kubeconfig, or fails when it is empty.
// extraAWS is spliced ahead of the defaults so a case can override one answer.
func checkStubs(t *testing.T, clusters, kubeconfig, extraAWS string) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, tool := range []string{"tofu", "terragrunt", "helm", "git", "gh"} {
		write(tool, "exit 0\n")
	}
	write("aws", `case "$*" in
`+extraAWS+`  *list-clusters*) `+clusters+` ;;
  *get-caller-identity*) echo 111111111111 ;;
  *head-bucket*) echo 'An error occurred (404) when calling the HeadBucket operation: Not Found' >&2; exit 254 ;;
esac
exit 0
`)
	kubectl := "exit 1\n"
	if kubeconfig != "" {
		kubectl = `case "$*" in
  *"config view"*) echo '` + kubeconfig + `' ;;
  *) exit 1 ;;
esac
`
	}
	write("kubectl", kubectl)
	return dir
}

// An unreachable cluster used to pass with exit 0 whatever the reason, so a CI runner with
// no kubeconfig entry gated a deploy on a platform nobody examined. EKS is asked whether the
// cluster exists: only "not yet" passes, and the report says which it was.
func TestCheck_AnUnreachableClusterPassesOnlyWhenThereIsNoClusterYet(t *testing.T) {
	for _, tc := range []struct {
		name, clusters, health, exists string
		passed                         bool
		code                           int
	}{
		{"no cluster yet", "echo False", "unreachable", "absent", true, ExitOK},
		{"present but unreachable", "echo True", "unreachable", "present", false, ExitUnhealthy},
		{"could not ask", "echo 'An error occurred (AccessDenied)' >&2; exit 254", "not-asserted", "unknown", false, ExitFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc, stderr, code := runCheckJSON(t, checkStubs(t, tc.clusters, "", ""), exampleConfig(t))
			if doc["clusterHealth"] != tc.health || doc["clusterExists"] != tc.exists || doc["passed"] != tc.passed {
				t.Errorf("clusterHealth=%v clusterExists=%v passed=%v, want %s %s %v\n%s",
					doc["clusterHealth"], doc["clusterExists"], doc["passed"], tc.health, tc.exists, tc.passed, stderr)
			}
			if code != tc.code {
				t.Errorf("exit %d, want %d\n%s", code, tc.code, stderr)
			}
		})
	}
}

// The same cluster name in another account or another region is a different cluster, and
// asserting its health under this config's title reports on a platform nobody asked about.
func TestCheck_TheSameNameInAnotherAccountOrRegionIsTheWrongCluster(t *testing.T) {
	for _, tc := range []struct{ name, entry, says string }{
		{"another account", "arn:aws:eks:us-west-2:222222222222:cluster/development-platform", "in account 222222222222"},
		{"another region", "arn:aws:eks:eu-west-1:111111111111:cluster/development-platform", "in region eu-west-1"},
		{"another name", "arn:aws:eks:us-west-2:111111111111:cluster/staging-platform", `"staging-platform"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc, stderr, code := runCheckJSON(t, checkStubs(t, "echo True", tc.entry, ""), exampleConfig(t))
			if doc["clusterHealth"] != "wrong-cluster" || doc["passed"] != false {
				t.Errorf("clusterHealth=%v passed=%v, want wrong-cluster and false\n%s", doc["clusterHealth"], doc["passed"], stderr)
			}
			if code != ExitUnhealthy {
				t.Errorf("exit %d, want %d\n%s", code, ExitUnhealthy, stderr)
			}
			if !strings.Contains(stderr, tc.says) {
				t.Errorf("the warning must say what differs (%q):\n%s", tc.says, stderr)
			}
		})
	}
}

// A check that ran and found only pre-spend failures exits preflight: the install would not
// succeed, and nothing about a platform failed. A caller can tell it from both "could not
// run" and "the platform is unhealthy".
func TestCheck_PreflightOnlyFailuresExitPreflight(t *testing.T) {
	// head-bucket answering 200 for a name this account does not own: the name is taken.
	taken := "  *head-bucket*) exit 0 ;;\n"
	doc, stderr, code := runCheckJSON(t, checkStubs(t, "echo False", "", taken), exampleConfig(t))
	if doc["passed"] != false || doc["clusterExists"] != "absent" {
		t.Errorf("passed=%v clusterExists=%v, want false and absent\n%s", doc["passed"], doc["clusterExists"], stderr)
	}
	if code != ExitPreflight {
		t.Errorf("exit %d, want %d\n%s", code, ExitPreflight, stderr)
	}
}

func TestKubeClusterMismatch(t *testing.T) {
	cfg := config.Default()
	cfg.Cluster.Name = "platform"
	cfg.Environment = config.EnvDev
	cfg.Cloud.AccountID = "111111111111"
	cfg.Cloud.Region = "us-west-2"
	name := cfg.ClusterName()
	for _, tc := range []struct {
		entry string
		match bool
	}{
		{"arn:aws:eks:us-west-2:111111111111:cluster/" + name, true},
		{"arn:aws:eks:us-west-2:222222222222:cluster/" + name, false},
		{"arn:aws:eks:us-east-1:111111111111:cluster/" + name, false},
		{"arn:aws:eks:us-west-2:111111111111:cluster/other", false},
		// Not an ARN: compared as itself, never parsed into a match.
		{name, true},
		{"kind-" + name, false},
		{"x:cluster/" + name, false},
	} {
		if got := kubeClusterMismatch(tc.entry, cfg) == ""; got != tc.match {
			t.Errorf("kubeClusterMismatch(%q) matched=%v, want %v", tc.entry, got, tc.match)
		}
	}
}
