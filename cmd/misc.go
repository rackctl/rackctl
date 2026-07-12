package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/engine"
	"github.com/rackctl/rackctl/internal/exec"
	"github.com/rackctl/rackctl/internal/phases"
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

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check prerequisites and platform health",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()
		run := exec.New(os.Stdout)

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

		if out, err := run.Capture(ctx, "kubectl", "get", "nodes", "--no-headers"); err == nil && out != "" {
			fmt.Println(ui.OK("cluster reachable"))
			if apps, err := run.Capture(ctx, "kubectl", "-n", "argocd", "get", "applications", "--no-headers"); err == nil && apps != "" {
				fmt.Println(ui.OK("argocd applications present"))
			} else {
				fmt.Println(ui.Warn("no argocd applications found"))
			}
		} else {
			fmt.Println(ui.Warn("no cluster in kubeconfig — run `rackctl init` first"))
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
		run.Env = []string{"AWS_PROFILE=" + cfg.Cloud.Profile, "AWS_REGION=" + cfg.Cloud.Region}
		run.Dir = engine.RepoPaths(cfg.Org.Name).LandingZone

		fmt.Println(ui.Title(fmt.Sprintf("rackctl destroy — %s · %s · %s", cfg.Org.Name, cfg.Cloud.Region, cfg.Environment)))
		if run.DryRun {
			fmt.Println(ui.Warn("dry-run — no cloud changes (pass --apply to destroy)"))
		}

		env := string(cfg.Environment)
		comps := phases.CoreComponents
		for i := len(comps) - 1; i >= 0; i-- {
			c := comps[i]
			fmt.Println(ui.Step("destroy " + c))
			if err := run.Run(ctx, "terragrunt", "--working-dir", "live/aws/workload-"+env+"/"+c,
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
		run.Dir = repos.EKSGitops
		fmt.Println(ui.Step("pulling latest eks-gitops addon catalog"))
		if err := run.Run(ctx, "git", "pull", "--ff-only"); err != nil {
			return err
		}
		run.Dir = repos.AgentPlatform
		fmt.Println(ui.Step("bumping the operator chart"))
		if err := run.Run(ctx, "helm", "upgrade", "--install", "operator", "oci://ghcr.io/nanohype/charts/operator"); err != nil {
			return err
		}
		fmt.Println(ui.OK("upgrade applied — ArgoCD will reconcile the catalog"))
		return nil
	},
}

func init() {
	destroyCmd.Flags().StringVarP(&destroyConfig, "config", "c", "rackctl.yaml", "path to rackctl.yaml")
	destroyCmd.Flags().BoolVar(&destroyApply, "apply", false, "actually destroy (default is a dry-run plan)")
	upgradeCmd.Flags().StringVarP(&upgradeConfig, "config", "c", "rackctl.yaml", "path to rackctl.yaml")
}
