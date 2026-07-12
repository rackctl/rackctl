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
	"github.com/rackctl/rackctl/internal/tui"
	"github.com/rackctl/rackctl/internal/ui"
)

var (
	initConfigPath string
	initApply      bool
	initNoClean    bool
	initTUI        bool
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Provision a nanohype platform from zero (full provision, AWS)",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(initConfigPath)
		if err != nil {
			return err
		}

		run := exec.New(os.Stdout)
		run.DryRun = !initApply
		// landing-zone's root.hcl resolves the account via TERRAGRUNT_ACCOUNT_ID
		// (its account.hcl is a placeholder), and the tfstate bucket is
		// {account}-{region}-tfstate — so terragrunt must see the real account.
		run.Env = []string{
			"AWS_PROFILE=" + cfg.Cloud.Profile,
			"AWS_REGION=" + cfg.Cloud.Region,
			"TERRAGRUNT_ACCOUNT_ID=" + cfg.Cloud.AccountID,
		}

		title := fmt.Sprintf("rackctl init — %s · %s · %s", cfg.Org.Name, cfg.Cloud.Region, cfg.Environment)
		st := &engine.State{Config: cfg, Runner: run}

		if initTUI {
			return tui.RunInit(title, st, phases.All())
		}

		fmt.Println(ui.Title(title))
		if run.DryRun {
			fmt.Println(ui.Warn("dry-run — no cloud changes (pass --apply to provision)"))
		}
		eng := &engine.Engine{Phases: phases.All(), Out: os.Stdout, CleanOnFail: !initNoClean}
		return eng.Run(context.Background(), st)
	},
}

func init() {
	f := initCmd.Flags()
	f.StringVarP(&initConfigPath, "config", "c", "rackctl.yaml", "path to rackctl.yaml")
	f.BoolVar(&initApply, "apply", false, "provision for real (default is a dry-run plan)")
	f.BoolVar(&initNoClean, "no-clean-on-failure", false, "leave resources in place if a phase fails")
	f.BoolVar(&initTUI, "tui", false, "interactive TUI progress view")
}
