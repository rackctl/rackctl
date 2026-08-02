package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/doctor"
	"github.com/rackctl/rackctl/internal/engine"
	"github.com/rackctl/rackctl/internal/exec"
	"github.com/rackctl/rackctl/internal/phases"
	"github.com/rackctl/rackctl/internal/preflight"
	"github.com/rackctl/rackctl/internal/reap"
	"github.com/rackctl/rackctl/internal/ui"
)

// Version is set at build time via -ldflags.
var Version = "0.0.0-dev"

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the rackctl version",
	Run:   func(cmd *cobra.Command, args []string) { fmt.Println("rackctl", Version) },
}

// ---- doctor ----

var doctorConfig string

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check prerequisites and assert platform health",
	Long: `Check prerequisites, then assert the invariants of a provisioned platform.

Exits non-zero if the platform is unhealthy, so it can gate a deploy.

Each platform check corresponds to a failure that has actually shipped a broken
cluster while every surface reported success: an app-of-apps syncing from the wrong
GitHub org, ApplicationSets erroring so silently they generated nothing to notice,
a metrics collector failing every write, dashboards that never rendered, and a
node pool tuned to evict half the fleet at once.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()
		run := exec.New(os.Stdout)

		// ── prerequisites ──
		if err := exec.RequireTools("tofu", "terragrunt", "kubectl", "helm", "aws", "git", "gh"); err != nil {
			fmt.Println(ui.Fail(err.Error()))
			return err
		}
		fmt.Println(ui.OK("required tools present"))

		if id, err := run.Capture(ctx, "aws", "sts", "get-caller-identity", "--query", "Account", "--output", "text"); err == nil && id != "" {
			fmt.Println(ui.OK("aws identity: account " + id))
		} else {
			fmt.Println(ui.Warn("aws identity unavailable — run `aws sso login`"))
		}

		// No cluster is a FAILURE, not a warning.
		//
		// This used to `return nil`, on the reasoning that "nothing provisioned yet is not
		// a failure". But doctor's job is to assert that a provisioned platform is healthy,
		// and it is the thing a deploy gates on — so an empty account exiting 0 means
		// "nothing exists" reports as HEALTHY to everything downstream. That is the same
		// green-light-that-means-nothing this command was written to eliminate.
		//
		// The pre-provision question — "can this install succeed?" — is `rackctl preflight`,
		// which needs no cluster and is where that check belongs.
		if out, err := run.Capture(ctx, "kubectl", "get", "nodes", "--no-headers"); err != nil || out == "" {
			fmt.Println(ui.Fail("no cluster in kubeconfig — there is no platform to be healthy. " +
				"Run `rackctl preflight` to check whether an install can succeed, then `rackctl init --apply`."))
			return fmt.Errorf("no provisioned platform to diagnose")
		}
		fmt.Println(ui.OK("cluster reachable"))

		// ── platform invariants ──
		// These need the config: several assertions are only meaningful against what
		// the operator ASKED for (which catalog is theirs, whether monitoring is on).
		cfg, err := config.Load(doctorConfig)
		if err != nil {
			fmt.Println(ui.Warn("no config — skipping platform checks (pass --config)"))
			return nil
		}

		fmt.Println()
		results := doctor.Run(ctx, &doctor.Env{Cfg: cfg, Run: run})
		printResults(results)

		if doctor.Failed(results) {
			fmt.Println()
			// Return an error so the process exits non-zero. The old doctor always
			// returned nil, which meant nothing could ever gate on its verdict.
			return fmt.Errorf("platform is unhealthy — see the failures above")
		}
		return nil
	},
}

// ---- preflight ----

var preflightConfig string

var preflightCmd = &cobra.Command{
	Use:   "preflight",
	Short: "Check whether an install can succeed — before it starts spending money",
	Long: `Assert, without a cluster, that this install CAN succeed.

doctor needs a live cluster, so it can only say why an install broke — never that
it was going to. Every failure of the first four provisioning runs was knowable in
advance and cost a full run to find: a globally-unique S3 bucket name already taken,
two components claiming one service account's Pod Identity, a fork that could not be
re-forked, Terraform state still describing a cluster that had been deleted, a KMS
alias left pointing at a key scheduled for deletion.

None of those are cloud failures. They are collisions with the wreckage of a previous
attempt, and a machine can enumerate them in seconds.

Read-only, and exits non-zero — so it can gate an init.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(preflightConfig)
		if err != nil {
			return err
		}
		ctx := context.Background()
		run := exec.New(io.Discard) // checks are queries; their output is the result, not the noise
		run.Env = []string{"AWS_PROFILE=" + cfg.Cloud.Profile, "AWS_REGION=" + cfg.Cloud.Region}

		fmt.Println(ui.Title(fmt.Sprintf("rackctl preflight — %s · %s · %s",
			cfg.Org.Name, cfg.Cloud.Region, cfg.Environment)))

		if err := exec.RequireTools("tofu", "terragrunt", "kubectl", "helm", "aws", "git", "gh"); err != nil {
			fmt.Println(ui.Fail(err.Error()))
			return err
		}
		fmt.Println(ui.OK("required tools present"))
		fmt.Println()

		results := preflight.Run(ctx, &preflight.Env{Cfg: cfg, Run: run})
		printResults(results)

		if preflight.Failed(results) {
			fmt.Println()
			return fmt.Errorf("preflight failed — this install would not succeed; clear the above first")
		}
		fmt.Println()
		fmt.Println(ui.OK("preflight clear — `rackctl init --apply` can proceed"))
		return nil
	},
}

// printResults renders check results identically for preflight and doctor — they differ
// in when they run, not in what they are.
func printResults(results []doctor.Result) {
	for _, r := range results {
		line := fmt.Sprintf("%-22s %s", r.Name, r.Detail)
		switch r.Status {
		case doctor.OK:
			fmt.Println(ui.OK(line))
		case doctor.Warn:
			fmt.Println(ui.Warn(line))
		case doctor.Fail:
			fmt.Println(ui.Fail(line))
		case doctor.Skip:
			fmt.Println(ui.Step(line))
		}
	}
}

// ---- destroy ----

var (
	destroyConfig       string
	destroyApply        bool
	destroyForceBuckets bool
)

var destroyCmd = &cobra.Command{
	Use:   "destroy",
	Short: "Tear down a provisioned platform (reverse order)",
	Long: `Tear down a provisioned platform in reverse apply order.

By default this is a dry-run plan. Pass --apply to destroy.

A dry-run makes READ-ONLY AWS calls. The three sweeps that delete resources outside
Terraform's state — operator-minted IAM roles, Karpenter's instances, orphaned EBS
volumes — each enumerate for real and print exactly what they would delete, and what
they would refuse to delete because nothing proves it belongs to this run. That is
the point of the dry-run: in an account holding more than this platform, "the sweep
is correctly scoped" is a claim worth checking rather than asserting, and a dry-run
that queried nothing could only ever restate its own filter back.

Outside development, several S3 buckets (agent-iam artifacts, cluster-addons
velero/loki/tempo, model-import staging, druid deepstorage) refuse a destroy
when non-empty unless force_destroy has been applied into state first. Pass
--force-buckets with --apply to do that two-act sequence: apply the owning
components with TF_VAR_force_destroy_buckets=true, then destroy. Development
always allows teardown without the flag.

Note: druid is NOT covered outside development (ledger O8). Its Aurora carries
deletion_protection = true, pinned in the staging and production leaves and
unreachable from a TF_VAR, so the permitting apply cannot clear it. rackctl
refuses rather than emptying the deepstorage segments and then wedging on
DeleteDBCluster — clear deletion_protection out of band first.

Note: eks-agent-platform's bedrock and cost-pipeline buckets (ledger O5) do not
yet accept force_destroy_buckets — a destroy after the platform has run may still
wedge there until that lands upstream.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(destroyConfig)
		if err != nil {
			return err
		}
		// --force-buckets needs no dry-run guard: PermitBucketTeardown prints both acts
		// and refuses where it must, so a dry-run is informative rather than dangerous.
		ctx := context.Background()
		run := exec.New(os.Stdout)
		run.DryRun = !destroyApply
		run.Env = tgEnv(cfg)
		run.Dir = engine.RepoPaths(cfg.Org.Name).LandingZone

		fmt.Println(ui.Title(fmt.Sprintf("rackctl destroy — %s · %s · %s", cfg.Org.Name, cfg.Cloud.Region, cfg.Environment)))
		if run.DryRun {
			fmt.Println(ui.Warn("dry-run — no cloud changes (pass --apply to destroy)"))
		}
		if destroyForceBuckets {
			fmt.Println(ui.Warn("force-buckets: will apply force_destroy_buckets=true on bucket-owning components before destroying"))
		}

		// Point kubectl at the cluster being destroyed, FIRST, and treat a failure as
		// disqualifying for the two sweeps that act on ambient Kubernetes state.
		//
		// reap.All and reap.UnstickTerminating run `kubectl delete
		// platforms|tenants|nodeclaims|pvc --all -A` and patch finalizers off CRs against
		// whatever context kubectl currently resolves. Their only guard is a `/readyz`
		// probe, which tests liveness, never identity. Nothing else in this command
		// touches the kubeconfig, and the environment passes through, so
		// `rackctl destroy --apply -c staging.yaml` from a shell pointed at a healthy
		// development cluster deleted every Platform, Tenant, NodeClaim and PVC in
		// DEVELOPMENT — and then stripped their finalizers.
		//
		// The engine's rollback path holds this invariant with State.KubeconfigCluster;
		// this command does not use the engine, so it has to establish the same fact
		// itself. Same posture as the cluster phase: repoint, and only claim the fact if
		// the repoint succeeded.
		//
		// It is not merely a guard — it is also a fix for the aimed-at-nothing case. An
		// operator whose context is stale would previously have had the sweeps silently
		// skip a cluster that WAS reachable under its own name, leaving the controller-owned
		// resources for the component teardown to trip over.
		reaping := true
		if err := reap.PointAt(ctx, run, cfg.ClusterName()); err != nil {
			reaping = false
			fmt.Println(ui.Warn(fmt.Sprintf(
				"could not point kubectl at %s (%v) — skipping the in-cluster reap. The component "+
					"teardown still runs, and terraform state is scoped by state key, so nothing "+
					"belonging to another cluster can be touched. But a Platform or PVC left behind "+
					"here may stop that teardown on a DeleteConflict or an in-use security group; "+
					"if so, fix the kubeconfig and re-run.", cfg.ClusterName(), err)))
		}

		// REFUSE before anything destructive if this hub has vended spoke clusters.
		//
		// An eks-fleet Cluster CR is not a Kubernetes object with a Kubernetes-shaped blast
		// radius: the composition hands it to provider-opentofu, which applies a whole
		// landing-zone module — a real EKS control plane, its VPC, its NAT gateways — and
		// the ClusterProviderConfig vends CROSS-ACCOUNT by design. This hub is the only
		// thing that knows those clusters exist, so destroying it orphans every one of
		// them: the control plane that could delete them is gone, their terraform state is
		// in a bucket rackctl never touches, and no rackctl command will enumerate them
		// again. They bill forever, in accounts this config does not even name.
		//
		// rackctl will not delete them either — tearing down someone else's cluster as a
		// side effect of tearing down this one is not a decision an installer gets to make.
		// So it stops and says what is there.
		if reaping {
			if spokes := reap.FleetSpokes(ctx, run); len(spokes) > 0 {
				return fmt.Errorf(
					"this cluster is an eks-fleet hub with %d spoke cluster(s) still vended:\n\n"+
						"    %s\n\n"+
						"Each one is a real EKS cluster — its own control plane, VPC and NAT gateways, "+
						"provisioned by provider-opentofu and often in another AWS account. This hub is "+
						"the only place they are tracked. Destroying it would strand them: Crossplane "+
						"goes with the cluster, their terraform state stays in the eks-fleet state "+
						"bucket, and nothing rackctl runs would ever find them again.\n\n"+
						"Delete the spokes first and let Crossplane tear them down:\n"+
						"    kubectl delete clusters.fleet.nanohype.dev --all -A --wait\n\n"+
						"Watch them go (a spoke takes as long as any EKS teardown), then re-run this "+
						"destroy. rackctl does not delete them for you — that is a different cluster's "+
						"lifecycle, and often a different account's bill",
					len(spokes), strings.Join(spokes, "\n    "))
			}
		}

		// Let the operator delete the AWS resources it — not Terraform — created,
		// while it is still running to do so. Destroying the cluster first orphans
		// them and makes agent-iam fail on DeleteConflict, halting the teardown with
		// the cluster already gone. See reap.go.
		if reaping {
			reap.All(ctx, run, os.Stdout)
		}

		// Force-delete the IAM roles the operator mints per Platform, in case its
		// finalizer did not (a crashlooping or already-pruned operator, or one stuck on
		// the node role). agent-iam destroys the tenant baseline policy those roles
		// attach; a survivor stops the whole teardown on DeleteConflict. This runs before
		// the component loop reaches agent-iam, and needs no cluster. See reap.go.
		//
		// Scoped to this cluster's roles by name. IAM is global and the operator's tenant
		// path carries no cluster segment, so an account running more than one cluster has
		// them all under one prefix — an unscoped sweep tears down a sibling cluster's live
		// tenant roles as a side effect of destroying this one.
		//
		// Scoped a third way as of this change: every candidate must carry a tag proving it
		// belongs to this run before it is force-deleted. The path and the name are structural
		// guesses; the tag is the resource's own statement. In an account that holds more than
		// this deployment, those are not the same thing.
		reap.OperatorRoles(ctx, run, os.Stdout, reap.Owner{Org: cfg.Org.Name, Cluster: cfg.ClusterName()})

		// With the roles gone, any Platform/Tenant still pinned in Terminating is
		// guarding nothing — free it, so an interrupted teardown does not wedge. Must
		// follow OperatorRoles (never orphan AWS state), and runs while the API is up.
		//
		// Gated on the same repoint, and here the gate is load-bearing in a way it was not
		// before OperatorRoles became cluster-scoped. Stripping a finalizer is only safe
		// because the roles it guards have already been force-deleted — that is the whole
		// argument in reap.go. Against the WRONG cluster, OperatorRoles now correctly
		// declines to touch that cluster's roles, so an ungated UnstickTerminating would
		// remove finalizers from CRs whose AWS state is deliberately still alive, orphaning
		// exactly what this package exists never to orphan. Narrowing the IAM sweep without
		// gating this one would have made the wrong-cluster case worse than it was.
		if reaping {
			reap.UnstickTerminating(ctx, run, os.Stdout)
		}

		// Backstop the NodeClaim reap above. It needs a reachable cluster and a live
		// Karpenter; a teardown is often run against neither. Any instance Karpenter
		// launched that survives into the component destroy holds the node security
		// group, and Terraform cannot delete a security group that is in use — the
		// teardown then stops with the cluster already gone and the instance still
		// billing. Runs BEFORE the components, unlike the volume sweep.
		reap.OrphanedNodes(ctx, run, os.Stdout, cfg.ClusterName(), cfg.Cloud.Region)

		// Reverse of the apply order, through the SAME helper the phases use.
		//
		// This loop used to restate terragrunt's init+destroy sequence and build its env from
		// tgEnv alone — so a standalone `rackctl destroy` passed none of the per-component
		// variables the apply had injected, and the cluster component fell back to its own
		// default name while its fail-closed endpoint precondition had nothing to satisfy it.
		// phases.Destroy owns both, so the two paths cannot drift.
		//
		// Every component is destroyed, with no exceptions. An earlier pass carved model-import
		// out as account-scoped substrate that must survive; landing-zone has since scoped it by
		// environment and given it a teardown posture (force_destroy unconditional in
		// development, opt-in elsewhere via force_destroy_buckets), so the carve-out is gone.
		// rackctl destroys everything it builds — if something must outlive a cluster, rackctl
		// should not be the thing creating it.
		st := &engine.State{
			Config:  cfg,
			Runner:  run,
			Repos:   engine.RepoPaths(cfg.Org.Name),
			Outputs: map[string]string{},
		}

		// Act 1 of the two-act teardown. force_destroy has no effect until an apply lands
		// it in state, so this must run BEFORE any destroy — and while every dependency each
		// leaf declares is still live (agent-iam needs cluster+secrets, cluster-addons needs
		// cluster, druid needs network+cluster, model-import needs nothing).
		if destroyForceBuckets {
			fmt.Println(ui.Step("force-buckets: permit teardown (apply force_destroy_buckets=true)"))
			if err := phases.PermitBucketTeardown(ctx, st); err != nil {
				return err
			}
		}

		// The agent-platform terraform tree comes down FIRST, before any landing-zone
		// component. Its components resolve landing-zone's SSM parameters — agent-iam's
		// operator role and tenant paths, observability's alert topic ARNs — through unguarded
		// data blocks, and Terraform evaluates data sources during a DESTROY plan too. Tearing
		// landing-zone down first leaves them unable to plan their own teardown at all.
		//
		// Its inputs are re-read here because a standalone destroy starts cold: nothing
		// populated State.Outputs. The values are still present — network, secrets and cluster
		// are precisely what the teardown has not reached yet — so this reads them back rather
		// than guessing or skipping.
		//
		// O5: bedrock and cost-pipeline still have no force_destroy_buckets. A destroy after
		// the platform has run may wedge on those four buckets until upstream ships the input.
		// Disclosed here so --force-buckets is not mistaken for covering that tree.
		if cfg.AgentPlatform.Enabled() {
			if destroyForceBuckets {
				fmt.Println(ui.Warn("force-buckets does not cover eks-agent-platform bedrock/cost-pipeline " +
					"(ledger O5) — those four buckets still have no force_destroy_buckets input"))
			}
			phases.CaptureLandingZoneOutputs(ctx, st)
			fmt.Println(ui.Step("destroy agent-platform substrate"))
			if err := phases.DestroyAgentPlatform(ctx, st); err != nil {
				return err
			}
		}

		comps := phases.CoreComponents(cfg)
		for i := len(comps) - 1; i >= 0; i-- {
			c := comps[i]
			fmt.Println(ui.Step("destroy " + c))
			if err := phases.Destroy(ctx, st, c); err != nil {
				return err
			}
		}
		// The cluster is gone; anything still tagged for it is an orphan by definition.
		reap.OrphanedVolumes(ctx, run, os.Stdout, cfg.ClusterName(), cfg.Cloud.Region)
		fmt.Println(ui.OK("platform destroyed"))
		return nil
	},
}

// ---- upgrade ----

var upgradeConfig string

var upgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Upgrade the platform to a newer nanohype release",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(upgradeConfig)
		if err != nil {
			return err
		}
		ctx := context.Background()
		run := exec.New(os.Stdout)
		run.Env = []string{"AWS_PROFILE=" + cfg.Cloud.Profile, "AWS_REGION=" + cfg.Cloud.Region}
		repos := engine.RepoPaths(cfg.Org.Name)

		fmt.Println(ui.Title("rackctl upgrade — " + cfg.Org.Name))

		// An upgrade is a git operation, not a helm one. The catalog is the source of
		// truth for everything running on the cluster — including the agent operator,
		// which the addons-agent-operator ApplicationSet installs from the
		// eks-agent-platform repo. Pull the catalog, push it, and ArgoCD reconciles.
		//
		// This used to also `helm upgrade --install operator
		// oci://ghcr.io/nanohype/charts/operator`, which (a) 403s — that chart is not
		// published to OCI — and (b) would install a SECOND, competing Helm release of
		// an operator ArgoCD already owns. Bumping the operator means bumping the chart
		// ref the catalog points at, not reaching around the catalog to install it.
		run.Dir = repos.EKSGitops
		fmt.Println(ui.Step("pulling the latest addon catalog"))
		if err := run.Run(ctx, "git", "pull", "--ff-only"); err != nil {
			return err
		}
		fmt.Println(ui.OK("catalog updated — ArgoCD will reconcile it, operator included"))
		return nil
	},
}

func init() {
	preflightCmd.Flags().StringVarP(&preflightConfig, "config", "c", "rackctl.yaml", "path to rackctl.yaml")
	doctorCmd.Flags().StringVarP(&doctorConfig, "config", "c", "rackctl.yaml", "path to rackctl.yaml")
	destroyCmd.Flags().StringVarP(&destroyConfig, "config", "c", "rackctl.yaml", "path to rackctl.yaml")
	destroyCmd.Flags().BoolVar(&destroyApply, "apply", false, "actually destroy (default is a dry-run plan)")
	destroyCmd.Flags().BoolVar(&destroyForceBuckets, "force-buckets", false,
		"before destroying, apply force_destroy_buckets=true on bucket-owning components so non-empty buckets can be emptied (two-act; required outside development for a reliable teardown, and does not cover druid there — see ledger O8)")
	upgradeCmd.Flags().StringVarP(&upgradeConfig, "config", "c", "rackctl.yaml", "path to rackctl.yaml")
}
