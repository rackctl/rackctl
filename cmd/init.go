package cmd

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/engine"
	"github.com/rackctl/rackctl/internal/exec"
	"github.com/rackctl/rackctl/internal/phases"
	"github.com/rackctl/rackctl/internal/preflight"
	"github.com/rackctl/rackctl/internal/tui"
	"github.com/rackctl/rackctl/internal/ui"
)

var (
	initConfigPath    string
	initApply         bool
	initNoClean       bool
	initTUI           bool
	initSkipPreflight bool
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
		run.Env = tgEnv(cfg)

		// Gate the spend.
		//
		// Every failure of the first four provisioning runs was knowable before a single
		// resource was created — a bucket name already taken in S3's global namespace,
		// state describing a cluster that had been deleted, a KMS alias orphaned by a
		// scheduled key deletion, a catalog fork one commit behind. Each cost a full run,
		// and several cost a teardown too.
		//
		// A preflight you have to remember to run is documentation, not a gate. So init
		// runs it, and refuses to spend money when it fails. --skip-preflight exists
		// because a check can be wrong and must never be the thing that blocks an
		// operator from their own cloud — but it has to be asked for.
		if initApply && !initSkipPreflight {
			if err := runPreflightGate(context.Background(), cfg); err != nil {
				return err
			}
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

// runPreflightGate asserts the install can succeed before it starts spending. It is
// deliberately quiet on success — the operator asked to provision, not to read an audit.
func runPreflightGate(ctx context.Context, cfg *config.Config) error {
	q := exec.New(io.Discard) // the checks are queries; their verdict is the output
	q.Env = []string{"AWS_PROFILE=" + cfg.Cloud.Profile, "AWS_REGION=" + cfg.Cloud.Region}

	results := preflight.Run(ctx, &preflight.Env{Cfg: cfg, Run: q})
	if !preflight.Failed(results) {
		fmt.Println(ui.OK("preflight clear"))
		return nil
	}

	fmt.Println(ui.Fail("preflight failed — refusing to provision"))
	fmt.Println()
	printResults(results)
	fmt.Println()
	return fmt.Errorf("this install would not succeed; clear the above (or pass --skip-preflight)")
}

func init() {
	f := initCmd.Flags()
	f.StringVarP(&initConfigPath, "config", "c", "rackctl.yaml", "path to rackctl.yaml")
	f.BoolVar(&initApply, "apply", false, "provision for real (default is a dry-run plan)")
	f.BoolVar(&initNoClean, "no-clean-on-failure", false, "leave resources in place if a phase fails")
	f.BoolVar(&initTUI, "tui", false, "interactive TUI progress view")
	f.BoolVar(&initSkipPreflight, "skip-preflight", false, "provision even if preflight says the install cannot succeed")
}
