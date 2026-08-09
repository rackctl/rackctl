package phases

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rackctl/rackctl/internal/engine"
)

// waitCatalogConverged blocks until every Application ArgoCD has generated is Healthy.
//
// It replaces `kubectl wait --for=jsonpath={.status.health.status}=Healthy applications
// --all`, which could not do this job for two independent reasons. Both were live.
//
// FIRST: `--all` matching NOTHING is a SUCCESS. kubectl prints "error: no matching
// resources found" to stderr and exits 0. cluster-bootstrap returns the moment ArgoCD's
// Deployment is up, which is before app-of-apps has generated a single child — so the wait
// looked at zero Applications and reported the catalog converged, instantly, every time.
// Observed on the first live install: the gate went green while the catalog stood at 3
// Healthy, 7 Missing, 4 Progressing.
//
// SECOND, and it survives fixing the first: `kubectl wait` resolves its resource set ONCE,
// at the start. The catalog does not exist all at once — app-of-apps generates children,
// and those ApplicationSets generate more, across sync waves 0→52. Any Application created
// after the wait begins is not in the set being waited on. So even starting from a non-empty
// catalog, the command answers a question about the Applications that happened to exist a
// second after ArgoCD started.
//
// So this polls, and it requires the set to be non-empty AND to have stopped growing:
// two consecutive samples with the same count and nothing unhealthy. Without the stability
// requirement the loop passes in the gap between waves, when wave N is Healthy and wave N+1
// has not been generated yet — which is the first defect wearing a different hat.
func waitCatalogConverged(ctx context.Context, st *engine.State, timeout time.Duration) error {
	const settle = 2 // consecutive clean samples at an unchanged count

	deadline := time.Now().Add(timeout)
	var lastCount, clean int
	var nextReport time.Time

	for time.Now().Before(deadline) {
		count, unhealthy, err := catalogHealth(ctx, st)
		if err == nil {
			switch {
			case count == 0:
				clean = 0
			case len(unhealthy) == 0 && count == lastCount:
				clean++
				if clean >= settle {
					note(st, "catalog converged: %d applications Healthy", count)
					return nil
				}
			default:
				clean = 0
			}
			lastCount = count
		}

		if time.Now().After(nextReport) {
			elapsed := int(time.Since(deadline.Add(-timeout)).Seconds())
			switch {
			case err != nil:
				note(st, "  ...%ds: cannot read applications yet", elapsed)
			case count == 0:
				note(st, "  ...%ds: app-of-apps has generated no children yet", elapsed)
			default:
				note(st, "  ...%ds: %d applications, %d not Healthy", elapsed, count, len(unhealthy))
			}
			nextReport = time.Now().Add(60 * time.Second)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}

	_, unhealthy, _ := catalogHealth(ctx, st)
	return fmt.Errorf("%d application(s) not Healthy: %s", len(unhealthy), strings.Join(unhealthy, " "))
}

// catalogHealth returns how many Applications exist and which are not Healthy.
//
// An error means the read failed, which is NOT the same as "zero applications" — a
// kubeconfig that cannot reach the API server would otherwise look exactly like a catalog
// that has not started, and the caller must not treat it as progress.
func catalogHealth(ctx context.Context, st *engine.State) (int, []string, error) {
	out, err := st.Runner.Capture(ctx, "kubectl", "-n", "argocd", "get", "applications",
		"-o", `jsonpath={range .items[*]}{.metadata.name}{"="}{.status.health.status}{"/"}{.status.sync.status}{"\n"}{end}`,
		"--request-timeout=30s")
	if err != nil {
		return 0, nil, err
	}
	var count int
	var unhealthy []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		count++
		name, state, _ := strings.Cut(line, "=")
		// An Application with no status yet reports "/" — not Healthy, and not something to
		// round up to it.
		if !strings.HasPrefix(state, "Healthy/") {
			unhealthy = append(unhealthy, name+" ("+state+")")
		}
	}
	return count, unhealthy, nil
}
