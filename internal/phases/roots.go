package phases

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rackctl/rackctl/internal/engine"
)

// componentGateRemedy maps a conditional component to the config change that turns it off.
// A guard that fires without naming the knob is a dead end for the operator reading it.
var componentGateRemedy = map[string]string{
	"agent-iam":          "set agentPlatform.enable: false (which also skips the agent operator wait in phase 6)",
	"managed-monitoring": "set addons.observability: false",
	"dns":                "remove the dns block",
	"model-import":       "set agentPlatform.modelImport: false",
}

// assertComponentRoots verifies that every landing-zone component this config will apply
// actually has a live root in the tree that was just cloned.
//
// The failure it prevents costs a full provision AND a full teardown.
//
// componentDir is a bare fmt.Sprintf — it composes a path and asserts nothing about it —
// and nothing in preflight stats a component directory, because preflight runs before the
// clone and has no tree to stat. So a component with no live root is discovered by
// terragrunt, in the substrate phase, several phases after the cluster phase built a VPC
// and an EKS control plane. terragrunt exits 1 immediately on a directory with no
// terragrunt.hcl, clean-on-failure is on by default, and the engine then tears the
// completed phases back down: the operator pays for a cluster, waits for it, and watches
// it be destroyed over a path that could have been checked in a millisecond.
//
// This is not hypothetical, and it is not about an exotic component. landing-zone's live
// tree is authored per environment, and agent-iam — which CoreComponents appends BY
// DEFAULT, because AgentPlatform.Enable nil means enabled — has a leaf only under
// workload-development. Every `rackctl init --apply` with environment: staging or
// production ends exactly this way today.
//
// The check is generic rather than a per-component environment allow-list on purpose.
// Hard-coding which environments carry which leaves would copy landing-zone's live-tree
// shape into this repo — the forking rackctl exists not to do — and would rot the moment
// the missing roots are added upstream. Statting what is actually there keeps working
// either way.
//
// Every failure is wrapped in engine.NoRollbackError, and that is load-bearing rather
// than defensive. This guard's entire premise is that NOTHING has been provisioned yet,
// so there is nothing for a rollback to reclaim — but the engine's teardown is not a
// no-op on an empty run. It force-deletes every IAM role under /eks-agent-platform/
// ACCOUNT-wide and runs `kubectl delete platforms|tenants|nodeclaims|pvc --all -A`
// against whatever cluster the kubeconfig currently points at. An operator bootstrapping
// staging from a laptop pointed at a healthy development cluster would have this guard
// fire correctly and then destroy the platform they were not touching. A precondition
// check must never be able to demolish something it exists to protect.
func assertComponentRoots(st *engine.State) error {
	comps := CoreComponents(st.Config)

	// "Is there a checkout?" must be answered the same way cloneOrUpdate answers it —
	// by the presence of .git, not of the directory. A path that exists but is not a
	// clone (an interrupted clone, a directory made by hand) is NOT a checkout, and
	// treating it as one made the guard fire on `network`, which every environment
	// carries, with a remedy that could not possibly help.
	_, err := os.Stat(filepath.Join(st.Repos.LandingZone, ".git"))
	cloned := err == nil

	// A dry-run's verdict can only ever be advisory, because a dry-run does not clone
	// and does not pull — acquire printed both. So the tree on disk is either absent or
	// arbitrarily old, and a hard failure would block a plan for a config that applies
	// perfectly once the pull actually happens. Report what is missing and let the plan
	// continue; the --apply, which pulls before this runs, is where it binds.
	if st.Runner.DryRun || !cloned {
		missing := missingRoots(st, comps)
		switch {
		case !cloned:
			dirs := make([]string, 0, len(comps))
			for _, c := range comps {
				dirs = append(dirs, componentDir(st, c))
			}
			note(st, "no landing-zone checkout yet — (apply) verifies a live root exists for each of: %s",
				strings.Join(dirs, ", "))
		case len(missing) > 0:
			note(st, "PLAN ONLY: this checkout has no live root for %s in the %s environment. A dry-run does "+
				"not pull, so the tree on disk predates whatever upstream has now — (apply) fast-forwards it "+
				"first and then fails hard if they are still missing", strings.Join(missing, ", "), st.Config.Environment)
		default:
			note(st, "every component this config applies has a live root in the checkout")
		}
		return nil
	}

	missing := missingRoots(st, comps)
	if len(missing) == 0 {
		return nil
	}
	c := missing[0]

	// A region with no live tree at all is a different diagnosis from a component with
	// no leaf, and the per-component remedy cannot fix it — componentDir embeds the
	// region as well as the environment, so `cloud.region` is the knob. Saying
	// "environment" here would send the operator after the wrong config field.
	regionRoot := filepath.Join(st.Repos.LandingZone, "live", "aws",
		"workload-"+string(st.Config.Environment), st.Config.Cloud.Region)
	if _, err := os.Stat(regionRoot); err != nil {
		return &engine.NoRollbackError{Err: fmt.Errorf(
			"landing-zone's live tree carries no roots at all for %s / %s:\n  %s\n\n"+
				"Every component would fail, starting with %q. landing-zone authors its live tree per "+
				"account, region and environment, and this combination has no leaves — so this is a "+
				"cloud.region (or environment) that landing-zone does not yet cover, not a component "+
				"problem. Either point cloud.region at a region the tree carries, or add the tree upstream. "+
				"Nothing has been provisioned, and nothing will be torn down",
			st.Config.Environment, st.Config.Cloud.Region, regionRoot, c)}
	}

	remedy, gated := componentGateRemedy[c]
	tail := fmt.Sprintf("Either %s, or add the live root upstream in landing-zone — for %s the leaf is a bare "+
		"root + _envcommon include with `inputs = {}`, and region and environment come from live/root.hcl, so "+
		"the file is byte-identical across environments.", remedy, c)
	if !gated {
		tail = fmt.Sprintf("%q is not optional and every environment's live tree carries it, so a checkout "+
			"missing it is almost certainly not a landing-zone clone — or the clone was interrupted. Check %s.",
			c, st.Repos.LandingZone)
	}

	return &engine.NoRollbackError{Err: fmt.Errorf(
		"landing-zone has no live root for the %q component in the %s environment (%s):\n  %s\n\n"+
			"rackctl applies that component with `terragrunt --working-dir <path> init`, and terragrunt exits 1 "+
			"immediately on a directory with no terragrunt.hcl. That failure would land in the substrate phase — "+
			"AFTER the cluster phase has already built a VPC and an EKS control plane — and clean-on-failure would "+
			"then tear both back down, so a missing live root costs a full provision AND a full teardown. It is "+
			"caught here instead, one phase after the clone and before anything is provisioned.\n\n%s",
		c, st.Config.Environment, st.Config.Cloud.Region,
		filepath.Join(st.Repos.LandingZone, componentDir(st, c), "terragrunt.hcl"), tail)}
}

// missingRoots returns the components with no terragrunt.hcl under the checkout, in
// apply order. It stats the FILE rather than the directory because terragrunt.hcl is
// exactly what terragrunt itself looks for — an empty directory left behind by an
// earlier run would pass a directory check and fail the apply anyway.
func missingRoots(st *engine.State, comps []string) []string {
	var missing []string
	for _, c := range comps {
		if _, err := os.Stat(filepath.Join(st.Repos.LandingZone, componentDir(st, c), "terragrunt.hcl")); err != nil {
			missing = append(missing, c)
		}
	}
	return missing
}
