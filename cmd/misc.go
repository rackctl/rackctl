package cmd

import (
	"context"
	"fmt"
	"os"

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

		if out, err := run.Capture(ctx, "kubectl", "get", "nodes", "--no-headers"); err != nil || out == "" {
			fmt.Println(ui.Warn("no cluster in kubeconfig — run `rackctl init` first"))
			return nil // nothing provisioned yet is not a failure
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

		if doctor.Failed(results) {
			fmt.Println()
			// Return an error so the process exits non-zero. The old doctor always
			// returned nil, which meant nothing could ever gate on its verdict.
			return fmt.Errorf("platform is unhealthy — see the failures above")
		}
		return nil
	},
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
		comps := phases.CoreComponents(cfg)
		for i := len(comps) - 1; i >= 0; i-- {
			c := comps[i]
			fmt.Println(ui.Step("destroy " + c))
			dir := fmt.Sprintf("live/aws/workload-%s/%s/%s/%s", env, cfg.Cloud.Region, env, c)
			if err := run.Run(ctx, "terragrunt", "--working-dir", dir,
				"--non-interactive", "destroy", "-auto-approve"); err != nil {
				return err
			}
		}
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
	doctorCmd.Flags().StringVarP(&doctorConfig, "config", "c", "rackctl.yaml", "path to rackctl.yaml")
	destroyCmd.Flags().StringVarP(&destroyConfig, "config", "c", "rackctl.yaml", "path to rackctl.yaml")
	destroyCmd.Flags().BoolVar(&destroyApply, "apply", false, "actually destroy (default is a dry-run plan)")
	upgradeCmd.Flags().StringVarP(&upgradeConfig, "config", "c", "rackctl.yaml", "path to rackctl.yaml")
}
