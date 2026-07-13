// Package doctor inspects a PROVISIONED platform and asserts the invariants that,
// when violated, produce a cluster that looks fine and is not.
//
// Every check here exists because the condition it tests was observed breaking a
// real install, and rackctl reported success anyway. The old `doctor` verified that
// the ArgoCD Application list was non-empty; it would have passed a cluster whose
// app-of-apps pointed at the wrong GitHub org, whose metrics collector was failing
// every write with a 403, whose dashboards had never rendered, and which had eight
// Degraded Applications — and it always exited 0, so nothing downstream could gate
// on it.
//
// The checks are deliberately independent and read-only: a doctor that mutates the
// thing it is diagnosing cannot be run against production.
package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/exec"
)

// Status is a check outcome. Warn never fails the run; Fail does.
type Status int

const (
	OK Status = iota
	Warn
	Fail
	Skip // not applicable to this config (e.g. dashboards when observability is off)
)

func (s Status) String() string {
	switch s {
	case OK:
		return "ok"
	case Warn:
		return "warn"
	case Fail:
		return "fail"
	default:
		return "skip"
	}
}

// Result is one check's outcome.
type Result struct {
	Name   string
	Status Status
	Detail string
}

func ok(name, d string) Result   { return Result{name, OK, d} }
func warn(name, d string) Result { return Result{name, Warn, d} }
func fail(name, d string) Result { return Result{name, Fail, d} }
func skip(name, d string) Result { return Result{name, Skip, d} }

// Env is what a check gets to work with.
type Env struct {
	Cfg *config.Config
	Run *exec.Runner
}

// kubectlJSON runs kubectl with -o json and unmarshals into v. A non-nil error
// means the cluster could not be read, not that the check failed.
func (e *Env) kubectlJSON(ctx context.Context, v any, args ...string) error {
	out, err := e.Run.Capture(ctx, "kubectl", append(args, "-o", "json")...)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) == "" {
		return fmt.Errorf("empty response")
	}
	return json.Unmarshal([]byte(out), v)
}

// Run executes every platform check and returns the results in a stable order.
// It does not stop at the first failure: a partial picture of a broken cluster is
// exactly what makes a broken cluster hard to diagnose.
func Run(ctx context.Context, env *Env) []Result {
	return []Result{
		CheckGitOpsSource(ctx, env),
		CheckApplications(ctx, env),
		CheckApplicationSets(ctx, env),
		CheckWorkloads(ctx, env),
		CheckDashboards(ctx, env),
		CheckKarpenter(ctx, env),
	}
}

// Failed reports whether any result is a Fail.
func Failed(rs []Result) bool {
	for _, r := range rs {
		if r.Status == Fail {
			return true
		}
	}
	return false
}

// ─────────────────────────── gitops source ───────────────────────────

// CheckGitOpsSource asserts the app-of-apps syncs from THIS ORG'S catalog fork.
//
// This is the check that would have caught the worst bug the tool has had. rackctl
// derived the fork, created the repo, printed its name — and never passed it to
// terragrunt, whose gitops_repo_url defaulted to the UPSTREAM catalog. So every
// install silently synced app-of-apps from someone else's main branch, unpinned,
// while the org's own fork sat unread. Nothing surfaced it: the cluster was healthy,
// the Applications were Synced, and they were Synced to the wrong repository.
func CheckGitOpsSource(ctx context.Context, env *Env) Result {
	const name = "gitops source"

	want := env.Cfg.Org.GitOps.GitURL()
	if want == "" {
		return warn(name, "org.gitops.eksGitopsRepo is not set — cannot verify the catalog source")
	}

	var app struct {
		Spec struct {
			Source struct {
				RepoURL        string `json:"repoURL"`
				TargetRevision string `json:"targetRevision"`
			} `json:"source"`
		} `json:"spec"`
	}
	if err := env.kubectlJSON(ctx, &app, "-n", "argocd", "get", "application", "app-of-apps"); err != nil {
		return fail(name, "app-of-apps not found — the gitops bootstrap did not complete")
	}

	got := app.Spec.Source.RepoURL
	if !sameRepo(got, want) {
		return fail(name, fmt.Sprintf(
			"app-of-apps syncs from %s but this org's catalog is %s — the cluster is reading someone else's repo, and edits to your fork will never take effect",
			got, want))
	}
	return ok(name, fmt.Sprintf("app-of-apps → %s @ %s", got, app.Spec.Source.TargetRevision))
}

// sameRepo compares two git URLs ignoring scheme, a .git suffix, and a trailing
// slash, so "github.com/a/b", "https://github.com/a/b" and
// "https://github.com/a/b.git" all match.
func sameRepo(a, b string) bool { return normalizeRepo(a) == normalizeRepo(b) }

func normalizeRepo(u string) string {
	u = strings.TrimSpace(u)
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimPrefix(u, "http://")
	u = strings.TrimPrefix(u, "git@")
	u = strings.ReplaceAll(u, ":", "/") // git@github.com:a/b -> github.com/a/b
	u = strings.TrimSuffix(u, "/")
	u = strings.TrimSuffix(u, ".git")
	return strings.ToLower(u)
}

// ─────────────────────────── argocd applications ───────────────────────────

type appList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Status struct {
			Health struct {
				Status string `json:"status"`
			} `json:"health"`
			Sync struct {
				Status string `json:"status"`
			} `json:"sync"`
		} `json:"status"`
	} `json:"items"`
}

// CheckApplications requires every Application to be Healthy AND Synced.
//
// The old doctor checked that the list was non-empty. A cluster with eight Degraded
// Applications — a crashlooping cost monitor, dashboards that never rendered, a
// GitOps controller stuck OutOfSync forever — passed that check.
//
// Synced matters as much as Healthy: an app that is Healthy but permanently
// OutOfSync is one whose selfHeal is fighting a controller it cannot win against,
// which means it is also not applying anything you commit.
func CheckApplications(ctx context.Context, env *Env) Result {
	const name = "argocd applications"

	var apps appList
	if err := env.kubectlJSON(ctx, &apps, "-n", "argocd", "get", "applications"); err != nil {
		return fail(name, "cannot read Applications — is ArgoCD installed?")
	}
	if len(apps.Items) == 0 {
		return fail(name, "no Applications — app-of-apps has not generated anything")
	}

	var bad []string
	for _, a := range apps.Items {
		h, s := a.Status.Health.Status, a.Status.Sync.Status
		if h != "Healthy" || s != "Synced" {
			bad = append(bad, fmt.Sprintf("%s (%s/%s)", a.Metadata.Name, h, s))
		}
	}
	sort.Strings(bad)

	if len(bad) > 0 {
		return fail(name, fmt.Sprintf("%d/%d not Healthy+Synced: %s",
			len(bad), len(apps.Items), strings.Join(bad, ", ")))
	}
	return ok(name, fmt.Sprintf("%d/%d Healthy + Synced", len(apps.Items), len(apps.Items)))
}

// ─────────────────────────── applicationsets ───────────────────────────

type appSetList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Status struct {
			Conditions []struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Message string `json:"message"`
			} `json:"conditions"`
		} `json:"status"`
	} `json:"items"`
}

// CheckApplicationSets requires no ApplicationSet to be in ErrorOccurred.
//
// An erroring ApplicationSet generates nothing, so it produces no Degraded
// Application to notice — the failure is invisible from the Application list. Three
// of them (a tenant appset pointing at private repos over SSH with no credential, a
// hub-only fleet appset running on a spoke, an app appset referencing AppProjects
// that did not exist) sat broken on a live cluster while every Application was
// green, and dragged app-of-apps to Degraded with no obvious cause.
func CheckApplicationSets(ctx context.Context, env *Env) Result {
	const name = "applicationsets"

	var sets appSetList
	if err := env.kubectlJSON(ctx, &sets, "-n", "argocd", "get", "applicationsets"); err != nil {
		return warn(name, "cannot read ApplicationSets")
	}

	var bad []string
	for _, s := range sets.Items {
		for _, c := range s.Status.Conditions {
			if c.Type == "ErrorOccurred" && c.Status == "True" {
				bad = append(bad, fmt.Sprintf("%s: %s", s.Metadata.Name, truncate(c.Message, 90)))
			}
		}
	}
	sort.Strings(bad)

	if len(bad) > 0 {
		return fail(name, fmt.Sprintf("%d erroring (they generate nothing, so no Application shows the failure):\n      %s",
			len(bad), strings.Join(bad, "\n      ")))
	}
	return ok(name, fmt.Sprintf("%d generating cleanly", len(sets.Items)))
}

// ─────────────────────────── workloads ───────────────────────────

type podList struct {
	Items []struct {
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Status struct {
			Phase             string `json:"phase"`
			ContainerStatuses []struct {
				Ready        bool `json:"ready"`
				RestartCount int  `json:"restartCount"`
				State        struct {
					Waiting *struct {
						Reason string `json:"reason"`
					} `json:"waiting"`
				} `json:"state"`
			} `json:"containerStatuses"`
		} `json:"status"`
	} `json:"items"`
}

// CheckWorkloads looks for pods that are crashlooping or wedged on config.
//
// The failures this catches were all "the pod exists, so the cluster looks up":
// the platform operator crashlooping on AssumeRoleWithWebIdentity 403 because its
// IAM role component was never applied; the metrics collector OOMKilled every few
// minutes; a cost monitor in CrashLoopBackOff because the metrics store it queries
// was empty. RestartCount is the tell — a pod can be Running right now and still
// have died forty times.
func CheckWorkloads(ctx context.Context, env *Env) Result {
	const name = "workloads"

	var pods podList
	if err := env.kubectlJSON(ctx, &pods, "get", "pods", "-A"); err != nil {
		return fail(name, "cannot read pods")
	}

	var bad []string
	for _, p := range pods.Items {
		if p.Status.Phase == "Succeeded" { // completed Jobs are fine
			continue
		}
		for _, c := range p.Status.ContainerStatuses {
			ref := p.Metadata.Namespace + "/" + p.Metadata.Name
			if w := c.State.Waiting; w != nil &&
				(w.Reason == "CrashLoopBackOff" || w.Reason == "CreateContainerConfigError" || w.Reason == "ImagePullBackOff") {
				bad = append(bad, fmt.Sprintf("%s: %s", ref, w.Reason))
				break
			}
			// Running but repeatedly killed — an OOMKilled collector looks healthy
			// in `get pods` between restarts.
			if c.RestartCount >= 5 {
				bad = append(bad, fmt.Sprintf("%s: %d restarts", ref, c.RestartCount))
				break
			}
		}
	}
	sort.Strings(bad)

	if len(bad) > 0 {
		return fail(name, fmt.Sprintf("%d unhealthy: %s", len(bad), strings.Join(bad, ", ")))
	}
	return ok(name, fmt.Sprintf("%d pods, none crashlooping", len(pods.Items)))
}

// ─────────────────────────── dashboards ───────────────────────────

type dashboardList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Status struct {
			Conditions []struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Message string `json:"message"`
			} `json:"conditions"`
		} `json:"status"`
	} `json:"items"`
}

// CheckDashboards requires every GrafanaDashboard to have actually rendered.
//
// Two distinct failures, both silent — the CRs exist, so nothing looks wrong:
//
//   - InvalidSpec: the dashboard's grafana.com id no longer exists upstream and the
//     operator 404s fetching it. Dashboard ids die; three had.
//   - ApplyFailed: the dashboard resolved but Grafana refused to save it. Amazon
//     Managed Grafana runs unified alerting and rejects any dashboard still carrying
//     a legacy panel `alert` block with a 500. One such dashboard held an entire
//     Application — and app-of-apps above it — Degraded.
func CheckDashboards(ctx context.Context, env *Env) Result {
	const name = "dashboards"

	if !env.Cfg.Addons.Observability {
		return skip(name, "addons.observability is off")
	}

	var ds dashboardList
	if err := env.kubectlJSON(ctx, &ds, "get", "grafanadashboards", "-A"); err != nil {
		return warn(name, "cannot read GrafanaDashboards — is grafana-operator installed?")
	}

	var bad []string
	for _, d := range ds.Items {
		for _, c := range d.Status.Conditions {
			broken := (c.Type == "InvalidSpec" && c.Status == "True") ||
				(c.Type == "DashboardSynchronized" && c.Status == "False")
			if broken {
				bad = append(bad, fmt.Sprintf("%s: %s", d.Metadata.Name, truncate(c.Message, 80)))
				break
			}
		}
	}
	sort.Strings(bad)

	if len(bad) > 0 {
		return fail(name, fmt.Sprintf("%d/%d never rendered:\n      %s",
			len(bad), len(ds.Items), strings.Join(bad, "\n      ")))
	}
	return ok(name, fmt.Sprintf("%d applied", len(ds.Items)))
}

// ─────────────────────────── karpenter ───────────────────────────

type nodePoolList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			Disruption struct {
				ConsolidationPolicy string `json:"consolidationPolicy"`
				ConsolidateAfter    string `json:"consolidateAfter"`
				Budgets             []struct {
					Nodes string `json:"nodes"`
				} `json:"budgets"`
			} `json:"disruption"`
		} `json:"spec"`
	} `json:"items"`
}

// CheckKarpenter warns when a NodePool is tuned so aggressively that routine
// consolidation becomes a fleet-wide disruption.
//
// This is not hypothetical tidiness. A pool with consolidateAfter: 1m and a
// disruption budget of 50% will, on a small cluster, evict half the fleet on a
// single consolidation decision — repeatedly. Observed as the entire node fleet
// cycling onto fresh spot capacity, taking the GitOps control plane with it, while
// every workload rescheduled onto nodes that were not yet Ready.
//
// A Warn, not a Fail: a large fleet can legitimately carry a percentage budget, and
// the doctor should not fail an install over a judgement call. But a budget above
// one node on a pool that also consolidates on a short fuse is worth naming.
func CheckKarpenter(ctx context.Context, env *Env) Result {
	const name = "karpenter"

	var pools nodePoolList
	if err := env.kubectlJSON(ctx, &pools, "get", "nodepools"); err != nil {
		return skip(name, "no NodePools (karpenter not installed)")
	}
	if len(pools.Items) == 0 {
		return skip(name, "no NodePools")
	}

	var risky []string
	for _, p := range pools.Items {
		d := p.Spec.Disruption
		if d.ConsolidationPolicy != "WhenEmptyOrUnderutilized" {
			continue // WhenEmpty only reclaims idle nodes; it cannot cascade
		}
		hairTrigger := isShortDuration(d.ConsolidateAfter)
		wide := false
		for _, b := range d.Budgets {
			if b.Nodes != "1" && b.Nodes != "0" {
				wide = true
			}
		}
		if hairTrigger && wide {
			risky = append(risky, fmt.Sprintf("%s (consolidateAfter=%s, budget=%s)",
				p.Metadata.Name, d.ConsolidateAfter, budgetsOf(d.Budgets)))
		}
	}

	if len(risky) > 0 {
		return warn(name, fmt.Sprintf(
			"%s — a short consolidation window with a multi-node disruption budget lets one consolidation decision cycle much of the fleet at once",
			strings.Join(risky, ", ")))
	}
	return ok(name, fmt.Sprintf("%d NodePool(s), disruption metered", len(pools.Items)))
}

// isShortDuration reports whether a Karpenter duration string is under ~5 minutes.
// Karpenter accepts values like "30s", "1m", "15m", "1h".
func isShortDuration(s string) bool {
	switch {
	case s == "":
		return false
	case strings.HasSuffix(s, "s"):
		return true // any seconds-scale window is a hair trigger
	case strings.HasSuffix(s, "m"):
		var m int
		if _, err := fmt.Sscanf(s, "%dm", &m); err != nil {
			return false
		}
		return m < 5
	default:
		return false // hours or unparseable — not short
	}
}

func budgetsOf(bs []struct {
	Nodes string `json:"nodes"`
}) string {
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.Nodes)
	}
	return strings.Join(out, ",")
}

// truncate collapses a (often multi-line) condition message to a single bounded
// line. It slices RUNES, not bytes: these messages carry em-dashes and quotes, and
// a byte slice can land mid-rune and emit invalid UTF-8.
func truncate(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
