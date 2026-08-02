package preflight

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/rackctl/rackctl/internal/doctor"
)

// CheckVersionSkew asserts the Kubernetes version this config asks for is one EKS will
// actually accept from where the cluster is now.
//
// This is the only genuinely dangerous thing about an upgrade, and it is entirely knowable
// beforehand. EKS upgrades the control plane ONE MINOR VERSION AT A TIME and has no downgrade
// at all. Neither rule is enforced anywhere in this stack: cluster.version rides
// TF_VAR_cluster_version straight into the cluster component, so a config that jumps two minors
// plans clean and fails in the apply — after the run has already reached the cluster phase.
//
// The failure is worse than a wasted apply. cluster.version is injected only when it differs
// from the default (S1's rule), so the operator who edits it is by definition the operator
// changing something, and the error AWS returns names neither the current version nor the
// sequence they need. A downgrade is worse still: it is not an error the operator can act on at
// all, because there is no way back.
//
// A fresh install has no skew — any supported version is a valid starting point — so no live
// cluster means nothing to check rather than a failure.
func CheckVersionSkew(ctx context.Context, env *Env) doctor.Result {
	const name = "version skew"

	want := strings.TrimSpace(env.Cfg.Cluster.Version)
	if want == "" {
		return ok(name, "no cluster.version pinned — landing-zone's default applies")
	}

	have, err := env.aws(ctx, "eks", "describe-cluster",
		"--name", clusterName(env.Cfg), "--query", "cluster.version")
	if err != nil || have == "" || have == "None" {
		return ok(name, "no live cluster — any supported version is a valid starting point")
	}

	hMaj, hMin, okH := parseMinor(have)
	wMaj, wMin, okW := parseMinor(want)
	if !okH || !okW {
		return warn(name, fmt.Sprintf("could not compare %q with %q", have, want))
	}

	switch {
	case hMaj == wMaj && hMin == wMin:
		return ok(name, "cluster is already on "+have)

	case wMaj < hMaj || (wMaj == hMaj && wMin < hMin):
		return fail(name, fmt.Sprintf(
			"the cluster is on %s and cluster.version asks for %s. **EKS cannot downgrade** — there "+
				"is no path back, and applying this does not fail safely so much as fail with nothing "+
				"you can do about it. Set cluster.version to %s or higher.", have, want, have))

	case wMaj == hMaj && wMin == hMin+1:
		return ok(name, fmt.Sprintf("%s → %s is a single minor step, which is what EKS allows", have, want))

	default:
		var steps []string
		for m := hMin + 1; m <= wMin; m++ {
			steps = append(steps, fmt.Sprintf("%d.%d", hMaj, m))
		}
		return fail(name, fmt.Sprintf(
			"the cluster is on %s and cluster.version asks for %s. EKS upgrades the control plane one "+
				"minor version at a time, so this jump is rejected — and it is rejected in the cluster "+
				"phase, after the run has started. Go through the versions in order, applying and "+
				"letting the nodes settle at each step: %s.",
			have, want, strings.Join(steps, " → ")))
	}
}

// parseMinor reads "1.36" (and tolerates a trailing patch, which EKS does not report but a
// hand-edited config might carry).
func parseMinor(v string) (int, int, bool) {
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".")
	if len(parts) < 2 {
		return 0, 0, false
	}
	maj, err1 := strconv.Atoi(parts[0])
	min, err2 := strconv.Atoi(parts[1])
	return maj, min, err1 == nil && err2 == nil
}
