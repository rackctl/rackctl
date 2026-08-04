package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/doctor"
	"github.com/rackctl/rackctl/internal/engine"
	"github.com/rackctl/rackctl/internal/exec"
	"github.com/rackctl/rackctl/internal/phases"
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

// printResults renders a check result. preflight and doctor produce the same shape because
// they differ in WHEN they run, not in what they are — which is also why `check` can show both
// sets under one heading without the operator having to know which is which.
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
	destroyDryRun       bool
	destroyYes          bool
	destroyForceBuckets bool
	// destroyAccountScoped permits removing the two eks-agent-platform roots under live/org.
	// Off by default because they are shared by every environment in the account, so a single
	// environment's teardown removing them is O14's failure arriving through the destroy door.
	destroyAccountScoped bool
)

var destroyCmd = &cobra.Command{
	Use:   "destroy",
	Short: "Tear down a provisioned platform (reverse order)",
	Long: `Tear down a provisioned platform in reverse apply order.

This writes. It asks for confirmation first unless --yes is passed; --dry-run shows
what it would do and touches nothing.

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

druid is covered: the permitting apply clears its Aurora deletion_protection in
the same act that lands force_destroy, so act 2 reaches both the per-tenant
buckets and the DB cluster.

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
		run.DryRun = destroyDryRun
		run.Env = tgEnv(cfg)
		run.Dir = engine.RepoPaths(cfg.Org.Name).LandingZone

		fmt.Println(ui.Title(fmt.Sprintf("rackctl destroy — %s · %s · %s", cfg.Org.Name, cfg.Cloud.Region, cfg.Environment)))
		if run.DryRun {
			fmt.Println(ui.Warn("dry-run — no cloud changes"))
		}
		if !run.DryRun {
			if err := confirmDestroy(cfg.ClusterName(), string(cfg.Environment)); err != nil {
				return err
			}
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
		// `rackctl destroy -c staging.yaml` from a shell pointed at a healthy
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
		// Two of that tree's roots are account-scoped and are NOT torn down by default — see
		// AgentPlatformTeardown. --account-scoped opts in; the phase names what it leaves or
		// takes either way.
		//
		// O5 has landed upstream, so --force-buckets now reaches that tree too: cost-pipeline
		// accepts force_destroy_buckets and bedrock-account derives force_destroy from its
		// object-lock mode, which live/org pins to GOVERNANCE for this reason.
		if cfg.AgentPlatform.Enabled() {
			if destroyAccountScoped && !destroyForceBuckets {
				fmt.Println(ui.Warn("--account-scoped without --force-buckets: the account buckets take " +
					"writes from the first PUT, so the destroy will wedge on BucketNotEmpty"))
			}
			phases.CaptureLandingZoneOutputs(ctx, st)
			fmt.Println(ui.Step("destroy agent-platform substrate"))
			if err := phases.DestroyAgentPlatform(ctx, st, phases.AgentPlatformTeardown{
				AccountScoped: destroyAccountScoped,
				ForceBuckets:  destroyForceBuckets,
			}); err != nil {
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

// confirmDestroy blocks until a human types the cluster name, or --yes was passed.
//
// destroy lost its --apply flag when the verbs became plan/apply/destroy, and something had to
// take over the job that flag was doing. A flag is the wrong thing for it anyway: --apply made
// the destructive invocation differ from the safe one by five characters at the END of a line,
// where it is read last and copied first.
//
// Typing the cluster name is a deliberately worse ergonomic than a y/n, because y/n is answered
// by reflex and the failure this guards is a reflex failure — tearing down the environment you
// meant to keep. The name is the one thing you cannot type correctly while thinking about a
// different cluster.
//
// --yes exists for CI, and for the campaign's own teardown loop, which must run unattended.
// Not a TTY and no --yes is a refusal rather than a silent proceed: a teardown that cannot ask
// is a teardown nobody authorised.
func confirmDestroy(cluster, env string) error {
	if destroyYes {
		return nil
	}
	fmt.Printf("\nThis destroys the %s platform %q and everything under it.\n", env, cluster)
	fmt.Printf("Type the cluster name to confirm: ")

	// Read the answer, and treat "there was nothing to read" as its own refusal.
	//
	// An os.Stdin.Stat() ModeCharDevice test is the usual way to ask "am I a terminal", and it
	// is wrong here: /dev/null IS a character device, so `rackctl destroy < /dev/null` sails
	// past the check and lands on a prompt nobody can answer. The outcome was still safe — an
	// empty answer does not match the cluster name — but the operator got "confirmation did not
	// match" for a situation where nothing was ever asked, and a misleading reason on a refusal
	// is how a guard gets worked around instead of understood.
	//
	// EOF answers the real question directly: not "is this a terminal" but "is there anyone
	// there to type". Scanln also returns an error for an empty line, so the two are separated
	// by whether anything was read at all.
	var typed string
	n, err := fmt.Scanln(&typed)
	if n == 0 && err != nil && strings.Contains(err.Error(), "EOF") {
		fmt.Println()
		return fmt.Errorf(
			"refusing to destroy %s: there is nobody to confirm with (stdin reached EOF). Pass "+
				"--yes if this is a scripted teardown, or --dry-run to see what it would do", cluster)
	}
	if strings.TrimSpace(typed) != cluster {
		return fmt.Errorf("confirmation did not match %q — nothing was destroyed", cluster)
	}
	fmt.Println()
	return nil
}

func init() {
	destroyCmd.Flags().StringVarP(&destroyConfig, "config", "c", "rackctl.yaml", "path to rackctl.yaml")
	destroyCmd.Flags().BoolVar(&destroyDryRun, "dry-run", false, "show what would be destroyed and touch nothing")
	destroyCmd.Flags().BoolVar(&destroyYes, "yes", false, "skip the confirmation prompt (for CI and scripted teardowns)")
	destroyCmd.Flags().BoolVar(&destroyForceBuckets, "force-buckets", false,
		"before destroying, apply force_destroy_buckets=true on bucket-owning components so non-empty buckets can be emptied (two-act; required outside development for a reliable teardown)")
	destroyCmd.Flags().BoolVar(&destroyAccountScoped, "account-scoped", false,
		"also destroy the account-scoped agent-platform roots (Bedrock invocation logging, the cost pipeline). They are SHARED by every environment in this account — only for the last one. Pair with --force-buckets")
}
