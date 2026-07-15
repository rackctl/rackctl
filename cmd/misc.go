package cmd

import (
	"context"
	"fmt"
	"io"
	"os"

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
	destroyConfig string
	destroyApply  bool
)

var destroyCmd = &cobra.Command{
	Use:   "destroy",
	Short: "Tear down a provisioned platform (reverse order)",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(destroyConfig)
		if err != nil {
			return err
		}
		ctx := context.Background()
		run := exec.New(os.Stdout)
		run.DryRun = !destroyApply
		run.Env = tgEnv(cfg)
		run.Dir = engine.RepoPaths(cfg.Org.Name).LandingZone

		fmt.Println(ui.Title(fmt.Sprintf("rackctl destroy — %s · %s · %s", cfg.Org.Name, cfg.Cloud.Region, cfg.Environment)))
		if run.DryRun {
			fmt.Println(ui.Warn("dry-run — no cloud changes (pass --apply to destroy)"))
		}

		// Let the operator delete the AWS resources it — not Terraform — created,
		// while it is still running to do so. Destroying the cluster first orphans
		// them and makes agent-iam fail on DeleteConflict, halting the teardown with
		// the cluster already gone. See reap.go.
		reap.All(ctx, run, os.Stdout)

		env := string(cfg.Environment)

		// Force-delete the IAM roles the operator mints per Platform, in case its
		// finalizer did not (a crashlooping or already-pruned operator, or one stuck on
		// the node role). agent-iam destroys the tenant baseline policy those roles
		// attach; a survivor stops the whole teardown on DeleteConflict. This runs before
		// the component loop reaches agent-iam, and needs no cluster. See reap.go.
		reap.OperatorRoles(ctx, run, os.Stdout)

		// With the roles gone, any Platform/Tenant still pinned in Terminating is
		// guarding nothing — free it, so an interrupted teardown does not wedge. Must
		// follow OperatorRoles (never orphan AWS state), and runs while the API is up.
		reap.UnstickTerminating(ctx, run, os.Stdout)

		// Backstop the NodeClaim reap above. It needs a reachable cluster and a live
		// Karpenter; a teardown is often run against neither. Any instance Karpenter
		// launched that survives into the component destroy holds the node security
		// group, and Terraform cannot delete a security group that is in use — the
		// teardown then stops with the cluster already gone and the instance still
		// billing. Runs BEFORE the components, unlike the volume sweep.
		reap.OrphanedNodes(ctx, run, os.Stdout, env+"-eks", cfg.Cloud.Region)

		comps := phases.CoreComponents(cfg)
		for i := len(comps) - 1; i >= 0; i-- {
			c := comps[i]
			fmt.Println(ui.Step("destroy " + c))
			dir := fmt.Sprintf("live/aws/workload-%s/%s/%s/%s", env, cfg.Cloud.Region, env, c)
			// init first — a destroy needs its modules installed exactly as much as an
			// apply does. A stale .terragrunt-cache (it lives in the checkout and
			// survives every run) makes tofu fail with "Module not installed" the moment
			// a component gains a module, and a teardown that cannot run is how a
			// half-built platform stays billing. See tg() in internal/phases.
			if err := run.Run(ctx, "terragrunt", "--working-dir", dir,
				"--non-interactive", "init"); err != nil {
				return err
			}
			if err := run.Run(ctx, "terragrunt", "--working-dir", dir,
				"--non-interactive", "destroy", "-auto-approve"); err != nil {
				return err
			}
		}
		// The cluster is gone; anything still tagged for it is an orphan by definition.
		reap.OrphanedVolumes(ctx, run, os.Stdout, env+"-eks", cfg.Cloud.Region)
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
	upgradeCmd.Flags().StringVarP(&upgradeConfig, "config", "c", "rackctl.yaml", "path to rackctl.yaml")
}
