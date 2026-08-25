package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/exec"
)

// kubectlEnv puts a fake `kubectl` on PATH that answers each `get <resource>` with a
// canned JSON document, and returns an Env driving the production exec.Runner through it.
//
// The fake is a real executable rather than an injected interface, matching the seam the
// rest of this repo uses: it exercises the argv these checks actually build, so a check
// that asks for the wrong resource fails here rather than on a live cluster.
func kubectlEnv(t *testing.T, byResource map[string]string) *Env {
	t.Helper()
	dir := t.TempDir()

	var b strings.Builder
	b.WriteString("#!/bin/sh\nfor a in \"$@\"; do case \"$a\" in\n")
	for resource, body := range byResource {
		b.WriteString(resource + ") cat <<'JSONEOF'\n" + body + "\nJSONEOF\n  exit 0 ;;\n")
	}
	b.WriteString("esac; done\nexit 1\n")

	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(b.String()), 0o755); err != nil {
		t.Fatalf("write fake kubectl: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := config.Default()
	cfg.Org.Name = "acme"
	cfg.Org.GitOps.EKSGitopsRepo = "github.com/acme/eks-gitops"
	cfg.Cluster.Name = "platform"
	return &Env{Cfg: cfg, Run: exec.New(&strings.Builder{})}
}

// A cluster syncing from the UPSTREAM catalog instead of the org's fork is the failure
// this check exists for: the fork rackctl created sits unread while every install tracks
// nanohype/eks-gitops@main.
func TestCheckGitOpsSource_FailsWhenTheClusterSyncsFromUpstream(t *testing.T) {
	env := kubectlEnv(t, map[string]string{"application": `{
	  "spec": {"source": {"repoURL": "https://github.com/nanohype/eks-gitops.git", "targetRevision": "main"}}
	}`})

	got := CheckGitOpsSource(t.Context(), env)
	if got.Status != Fail {
		t.Fatalf("a cluster pointed at the upstream catalog must fail; got %v: %s", got.Status, got.Detail)
	}
}

func TestCheckGitOpsSource_PassesOnTheOrgFork(t *testing.T) {
	env := kubectlEnv(t, map[string]string{"application": `{
	  "spec": {"source": {"repoURL": "https://github.com/acme/eks-gitops.git", "targetRevision": "main"}}
	}`})

	if got := CheckGitOpsSource(t.Context(), env); got.Status != OK {
		t.Fatalf("the org's own fork must pass; got %v: %s", got.Status, got.Detail)
	}
}

// An absent app-of-apps means the bootstrap never completed, which is a failure rather
// than an unreadable cluster.
func TestCheckGitOpsSource_FailsWhenAppOfAppsIsAbsent(t *testing.T) {
	env := kubectlEnv(t, nil)

	if got := CheckGitOpsSource(t.Context(), env); got.Status != Fail {
		t.Fatalf("a missing app-of-apps must fail; got %v: %s", got.Status, got.Detail)
	}
}

// An erroring ApplicationSet generates nothing, so NO Application shows the failure —
// which is exactly why it needs its own check rather than being inferred from app health.
func TestCheckApplicationSets_FailsOnErrorOccurred(t *testing.T) {
	env := kubectlEnv(t, map[string]string{"applicationsets": `{"items": [
	  {"metadata": {"name": "addons"},
	   "status": {"conditions": [{"type": "ErrorOccurred", "status": "True", "message": "template error"}]}}
	]}`})

	got := CheckApplicationSets(t.Context(), env)
	if got.Status != Fail {
		t.Fatalf("an erroring ApplicationSet must fail; got %v: %s", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, "addons") {
		t.Errorf("the detail must name the erroring set.\ngot: %s", got.Detail)
	}
}

func TestCheckApplicationSets_PassesWhenAllGenerate(t *testing.T) {
	env := kubectlEnv(t, map[string]string{"applicationsets": `{"items": [
	  {"metadata": {"name": "addons"}, "status": {"conditions": []}}
	]}`})

	if got := CheckApplicationSets(t.Context(), env); got.Status != OK {
		t.Fatalf("a cleanly generating set must pass; got %v: %s", got.Status, got.Detail)
	}
}

// An unreadable cluster is a warning, never a pass: "I could not look" and "there is
// nothing wrong" are the two answers a health check must never merge.
func TestCheckApplicationSets_UnreadableIsAWarningNotAPass(t *testing.T) {
	env := kubectlEnv(t, nil)

	if got := CheckApplicationSets(t.Context(), env); got.Status != Warn {
		t.Fatalf("an unreadable ApplicationSet list must warn, not pass; got %v", got.Status)
	}
}

func TestCheckWorkloads_FailsOnACrashloopingPod(t *testing.T) {
	env := kubectlEnv(t, map[string]string{"pods": `{"items": [
	  {"metadata": {"name": "opencost-0", "namespace": "opencost"},
	   "status": {"phase": "Running", "containerStatuses": [
	     {"restartCount": 12, "state": {"waiting": {"reason": "CrashLoopBackOff"}}}]}}
	]}`})

	got := CheckWorkloads(t.Context(), env)
	if got.Status != Fail {
		t.Fatalf("a crashlooping pod must fail; got %v: %s", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, "opencost") {
		t.Errorf("the detail must name the pod.\ngot: %s", got.Detail)
	}
}

func TestCheckWorkloads_PassesOnAHealthyFleet(t *testing.T) {
	env := kubectlEnv(t, map[string]string{"pods": `{"items": [
	  {"metadata": {"name": "argocd-server-0", "namespace": "argocd"},
	   "status": {"phase": "Running", "containerStatuses": [{"restartCount": 0, "state": {"running": {}}}]}}
	]}`})

	if got := CheckWorkloads(t.Context(), env); got.Status != OK {
		t.Fatalf("a healthy fleet must pass; got %v: %s", got.Status, got.Detail)
	}
}

// A completed Job is not an unhealthy workload.
func TestCheckWorkloads_IgnoresSucceededPods(t *testing.T) {
	env := kubectlEnv(t, map[string]string{"pods": `{"items": [
	  {"metadata": {"name": "migrate-abc", "namespace": "app"}, "status": {"phase": "Succeeded"}}
	]}`})

	if got := CheckWorkloads(t.Context(), env); got.Status == Fail {
		t.Fatalf("a Succeeded pod is a finished Job, not a failure: %s", got.Detail)
	}
}

// The floor tier runs no AMP or AMG, so there is nothing to assert — a skip, not a pass
// and not a warning about a component that was never meant to exist.
func TestCheckDashboards_SkippedOnTheFloorTier(t *testing.T) {
	env := kubectlEnv(t, nil)
	env.Cfg.Observability.Tier = config.TierFloor

	if got := CheckDashboards(t.Context(), env); got.Status != Skip {
		t.Fatalf("the floor tier has no dashboards to check; got %v: %s", got.Status, got.Detail)
	}
}

func TestCheckDashboards_FailsOnADashboardThatNeverRendered(t *testing.T) {
	env := kubectlEnv(t, map[string]string{"grafanadashboards": `{"items": [
	  {"metadata": {"name": "cluster-overview"},
	   "status": {"conditions": [{"type": "DashboardSynchronized", "status": "False",
	     "reason": "ApplyFailed", "message": "Dashboard failed to be applied for 1 out of 1 instances"}]}}
	]}`})

	got := CheckDashboards(t.Context(), env)
	if got.Status != Fail {
		t.Fatalf("a dashboard that never rendered must fail; got %v: %s", got.Status, got.Detail)
	}
}

// A short consolidation window PLUS a multi-node budget is what turns routine
// consolidation into a fleet-wide eviction. Either alone is fine, so the check must warn
// only on the combination.
func TestCheckKarpenter_WarnsOnAHairTriggerWithAWideBudget(t *testing.T) {
	env := kubectlEnv(t, map[string]string{"nodepools": `{"items": [
	  {"metadata": {"name": "sandbox"},
	   "spec": {"disruption": {"consolidationPolicy": "WhenEmptyOrUnderutilized",
	     "consolidateAfter": "30s", "budgets": [{"nodes": "10%"}]}}}
	]}`})

	got := CheckKarpenter(t.Context(), env)
	if got.Status != Warn {
		t.Fatalf("a 30s window with a multi-node budget must warn; got %v: %s", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, "sandbox") {
		t.Errorf("the detail must name the pool.\ngot: %s", got.Detail)
	}
}

func TestCheckKarpenter_QuietOnAMeteredPool(t *testing.T) {
	env := kubectlEnv(t, map[string]string{"nodepools": `{"items": [
	  {"metadata": {"name": "general"},
	   "spec": {"disruption": {"consolidationPolicy": "WhenEmptyOrUnderutilized",
	     "consolidateAfter": "15m", "budgets": [{"nodes": "1"}]}}}
	]}`})

	if got := CheckKarpenter(t.Context(), env); got.Status != OK {
		t.Fatalf("a metered pool must pass; got %v: %s", got.Status, got.Detail)
	}
}

// No Karpenter is not a failure — plenty of clusters do not run it.
func TestCheckKarpenter_SkippedWhenNotInstalled(t *testing.T) {
	env := kubectlEnv(t, nil)

	if got := CheckKarpenter(t.Context(), env); got.Status != Skip {
		t.Fatalf("an absent Karpenter must skip, not fail; got %v: %s", got.Status, got.Detail)
	}
}

// Run must execute every check and never stop at the first failure: a partial picture of
// a broken cluster is exactly what makes a broken cluster hard to diagnose.
func TestRun_ReturnsEveryCheckEvenWhenTheClusterIsUnreadable(t *testing.T) {
	env := kubectlEnv(t, nil)

	got := Run(t.Context(), env)
	if len(got) != 7 {
		t.Fatalf("Run must return all seven checks; got %d", len(got))
	}

	seen := map[string]bool{}
	for _, r := range got {
		if r.Name == "" {
			t.Errorf("a check returned an unnamed result: %+v", r)
		}
		if seen[r.Name] {
			t.Errorf("two checks share the name %q — a duplicate hides one of them", r.Name)
		}
		seen[r.Name] = true
	}
}

// Failed() is what decides `rackctl check`'s exit status, so Warn must not fail a run and
// Fail must.
func TestFailed_OnlyFailCountsAsAFailure(t *testing.T) {
	if Failed([]Result{{Name: "a", Status: Warn}, {Name: "b", Status: OK}, {Name: "c", Status: Skip}}) {
		t.Error("a warning is not a failed run — it is a thing to look at, not a reason to exit non-zero")
	}
	if !Failed([]Result{{Name: "a", Status: OK}, {Name: "b", Status: Fail}}) {
		t.Error("a Fail must fail the run")
	}
}
