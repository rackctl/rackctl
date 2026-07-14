package cmd

import (
	"strconv"

	"github.com/rackctl/rackctl/internal/config"
)

// tgEnv builds the environment every terragrunt invocation runs with.
//
// landing-zone's root.hcl resolves the account via TERRAGRUNT_ACCOUNT_ID (its
// account.hcl is a placeholder) and the tfstate bucket is
// {account}-{region}-tfstate, so terragrunt must see the real account.
//
// TF_VAR_gitops_repo_url is the one that matters most, and its absence was a real
// bug: cluster-bootstrap's gitops_repo_url used to default to the UPSTREAM catalog
// (nanohype/eks-gitops), and rackctl never passed a value — it only printed the
// fork's name in a log line. So every install wired its app-of-apps to upstream
// main, unpinned, while the fork rackctl had just created for the org sat unread.
// A cluster vended into another org was found syncing from nanohype/eks-gitops@main.
//
// landing-zone now declares gitops_repo_url with NO default, so a missing value
// fails the plan instead of silently borrowing someone else's catalog. Passing it
// here is what satisfies that. Terragrunt/tofu pick up TF_VAR_* automatically, so
// this needs no terragrunt.hcl inputs block and stays correct for every component.
// TF_VAR_enable_managed_monitoring is the same bug in a second place, found the same
// way — by looking at what a finished cluster was actually missing.
//
// cluster-bootstrap stamps the AMG workspace URL (and the AMP endpoint + workspace id)
// onto the ArgoCD cluster Secret, which is the ONLY channel by which per-cluster values
// reach the catalog. But it stamps them behind `var.enable_managed_monitoring`, which
// defaults to false — deliberately, because a cluster that does not run
// managed-monitoring has no SSM parameters to read and a default-true would fail its
// plan.
//
// rackctl knew observability was on. It ran the whole managed-monitoring component. It
// created the AMP and AMG workspaces and published all three SSM parameters. And then it
// never told cluster-bootstrap any of that had happened. So the annotations were never
// stamped, the dashboards ApplicationSet had nothing to inject, and the Grafana CR was
// rejected by its own CRD:
//
//	spec.external.url: Invalid value: "": in body should match '^https?://.+$'
//
// The dashboards Application sat Degraded on a cluster where every other Application was
// Healthy, the AMG workspace was up, and the token was valid. The variable's own
// description had predicted it: "Left false, the dashboards Grafana CR renders without
// an external URL."
//
// The lesson generalises past this flag: any landing-zone variable that is opt-in
// BECAUSE it depends on another component having run is a variable rackctl must supply,
// because rackctl is the only thing that knows which components it ran.
func tgEnv(cfg *config.Config) []string {
	env := []string{
		"AWS_PROFILE=" + cfg.Cloud.Profile,
		"AWS_REGION=" + cfg.Cloud.Region,
		"TERRAGRUNT_ACCOUNT_ID=" + cfg.Cloud.AccountID,
		// Safe to pass unconditionally: it is exactly CoreComponents' own condition for
		// including managed-monitoring, so it is true iff the SSM parameters exist.
		"TF_VAR_enable_managed_monitoring=" + strconv.FormatBool(cfg.Addons.Observability),
	}
	if u := cfg.Org.GitOps.GitURL(); u != "" {
		env = append(env, "TF_VAR_gitops_repo_url="+u)
	}
	return env
}
