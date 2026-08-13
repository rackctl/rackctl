package cmd

import (
	"strconv"

	"github.com/rackctl/rackctl/internal/config"
)

// awsEnv is the AWS identity every rackctl subprocess must run under.
//
// Composed in one place because it was previously composed in three, and one of
// them forgot: `rackctl check` pinned the profile on its preflight runner and not
// on the runner it handed to doctor, so the pre-spend checks asked about the
// configured account while the health checks asked about whatever the ambient
// environment happened to be. Identity that is assembled per call site is
// identity that will eventually disagree with itself.
func awsEnv(cfg *config.Config) []string {
	return []string{
		"AWS_PROFILE=" + cfg.Cloud.Profile,
		"AWS_REGION=" + cfg.Cloud.Region,
	}
}

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
	env := append(awsEnv(cfg),
		"TERRAGRUNT_ACCOUNT_ID="+cfg.Cloud.AccountID,
		// Both derived from the one tier field, so they cannot disagree.
		//
		// observability_tier is published as the observability/tier label on the ArgoCD cluster
		// Secret, and every tier-aware eks-gitops ApplicationSet either selects on it or derives
		// a value from it. Every committed cluster-bootstrap leaf pins it to full, so passing it
		// changes nothing in the common case and is what makes `tier: floor` reachable at all.
		//
		// enable_managed_monitoring is a second, wider switch on the same Secret. It is not just
		// the Grafana URL: it un-guards five SSM reads and stamps the `monitoring/managed` label
		// that the opencost ApplicationSet selects on as its ONLY selector, plus the
		// monitoring/* annotations and the Loki and Tempo bucket annotations that decide whether
		// those two get durable S3 storage or fall back to filesystem.
		//
		// So the safety argument is not "managed-monitoring ran". Two of those five parameters —
		// loki_bucket and tempo_bucket — are published by CLUSTER-ADDONS, not managed-monitoring.
		// Setting this flag is safe because the substrate phase applies BOTH components before
		// the gitops phase runs cluster-bootstrap, which is the same ordering the committed
		// leaves express as an explicit dependency. That phase boundary is the precondition; the
		// tier is only what decides whether it is wanted.
		//
		// enable_accelerators is deliberately absent. It labelled the cluster
		// eks-agent-platform/accelerators=true so the accelerators ApplicationSet targeted it,
		// and the whole GPU path — that ApplicationSet, the accelerator-pools component, and
		// landing-zone's variable and label — was deleted upstream (ledger O27). The variable is
		// now undeclared, so injecting it would be inert rather than wrong; it is gone anyway,
		// because a knob whose documentation promises an effect it no longer has is worse than
		// no knob. addons.accelerators is refused at config load, not ignored — see
		// internal/config/retired.go.
		//
		// enable_agent_platform labels the cluster into the operator ApplicationSet. Default
		// true upstream, which is the right day-0 answer — but when the operator opts out
		// (agentPlatform.enable: false) rackctl also skips agent-iam, so without this flag the
		// GitOps install still deploys an operator whose IAM role was never created.
		//
		// enable_external_dns stamps the domain-filter annotation from the SSM parameter the
		// dns component publishes. Every committed workload leaf pins it true, and the
		// development leaf declares a hard dependency on dns — so a config with no dns: block
		// fails cluster-bootstrap on a missing SSM parameter. Deriving the flag from whether
		// rackctl applied dns is what closes that hole.
		//
		// enable_portal_reader mints the portal's read-only ServiceAccount and a DURABLE
		// token, so the portal can register the cluster and watch Platform/Tenant CRs.
		//
		// It defaults to TRUE upstream (cluster-bootstrap/variables.tf:154) and no leaf pins
		// it, so a portal-enabled install already got the reader — this injection does not
		// fix an inert knob. What it does is the opposite, and deliberately: it turns the
		// reader OFF when controlPlane.portal is false, because a durable cluster-read token
		// that nothing consumes is a standing credential minted for no reason.
		//
		// This is the one place rackctl injects a value that is not "different from the
		// leaf's" — there is no leaf pin to respect here, only a component default, and
		// leaving a credential lying around is not a default worth inheriting.
		"TF_VAR_observability_tier="+string(cfg.Observability.Tier),
		"TF_VAR_enable_managed_monitoring="+strconv.FormatBool(cfg.FullObservability()),
		"TF_VAR_enable_agent_platform="+strconv.FormatBool(cfg.AgentPlatform.Enabled()),
		"TF_VAR_enable_external_dns="+strconv.FormatBool(cfg.HasDNS()),
		"TF_VAR_enable_portal_reader="+strconv.FormatBool(cfg.ControlPlane.Portal),
	)
	if u := cfg.Org.GitOps.GitURL(); u != "" {
		env = append(env, "TF_VAR_gitops_repo_url="+u)
	}
	// The catalog pin's second half, and the half that makes it real.
	//
	// versions.eksGitops decides which commit rackctl reads LOCALLY. On its own that
	// changes nothing about the cluster: ArgoCD clones the catalog itself. This is what
	// carries the pin across — cluster-bootstrap takes gitops_repo_branch, sets
	// app-of-apps' targetRevision from it, and stamps it on the ArgoCD cluster Secret as
	// gitops/repo-branch, which every ApplicationSet now templates its own targetRevision
	// from. Without this line a pinned install builds from a tag and then syncs a fleet
	// from main.
	if rev := cfg.Versions.EKSGitops; rev != "" {
		env = append(env, "TF_VAR_gitops_repo_branch="+rev)
	}
	return env
}
