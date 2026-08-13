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

// plan and apply are one pipeline behind two verbs, and the verb — not a flag — is what
// decides whether anything is written.
//
// A single command carrying a --apply flag makes the safe mode the one you get by forgetting,
// which sounds protective and is not: it means the dangerous invocation differs from the safe
// one by four characters at the end of a line, and the two are indistinguishable in a shell
// history, a runbook or a CI file until you read to the end. Naming the intent as the verb puts
// it first, where it is read.
//
// It also removes a genuine confusion. The provisioning verb used to be `init`, which every
// reader of terraform or git takes to mean the cheap, local, idempotent thing you run without
// thinking. Here it built a VPC and an EKS control plane and spent real money.
var (
	applyConfigPath    string
	applyNoClean       bool
	applyTUI           bool
	applySkipPreflight bool
)

var planCmd = &cobra.Command{
	Use:   "plan",
	Short: "Show what a provision would do, without touching anything",
	Long: `Walk every phase and print the commands a provision would run.

Read-only. It creates nothing, changes nothing, and needs no cluster.

It does make read-only AWS calls, and that is the point rather than a side effect: the
sweeps that delete resources outside Terraform's state — operator-minted IAM roles,
Karpenter's instances, orphaned EBS volumes — enumerate for real and print what they
would select. A plan that queried nothing could only ever restate its own filters back.`,
	RunE: func(cmd *cobra.Command, args []string) error { return runPipeline(false) },
}

var applyCmd = &cobra.Command{
	Use:   "apply",
	Short: "Provision a nanohype platform from zero (AWS)",
	Long: `Provision a platform from zero: network, EKS, the AWS substrate, ArgoCD and the
agent platform, in that order.

This writes. It creates real infrastructure and spends real money. Run ` + "`rackctl plan`" + `
first if you want to see what it would do.

Re-runnable by design — it is how you retry after a failure and how you re-apply a config
change. A run that finds the platform already standing will not tear it down: rollback is
only ever safe when this run built the thing it is about to destroy.

Preflight runs first and refuses to spend when it fails. A preflight you have to remember
to run is documentation, not a gate.`,
	RunE: func(cmd *cobra.Command, args []string) error { return runPipeline(true) },
}

func runPipeline(write bool) error {
	cfg, err := config.Load(applyConfigPath)
	if err != nil {
		return err
	}

	run := exec.New(os.Stdout)
	run.DryRun = !write
	run.Env = tgEnv(cfg)

	// Gate the spend.
	//
	// Every failure of the first four provisioning runs was knowable before a single resource
	// was created — a bucket name already taken in S3's global namespace, state describing a
	// cluster that had been deleted, a KMS alias orphaned by a scheduled key deletion, a
	// catalog fork one commit behind. Each cost a full run, and several cost a teardown too.
	//
	// --skip-preflight exists because a check can be wrong and must never be the thing that
	// stands between an operator and their own cloud — but it has to be asked for.
	if write && !applySkipPreflight {
		if err := runPreflightGate(context.Background(), cfg); err != nil {
			return err
		}
	}

	verb := "plan"
	if write {
		verb = "apply"
	}
	title := fmt.Sprintf("rackctl %s — %s · %s · %s", verb, cfg.Org.Name, cfg.Cloud.Region, cfg.Environment)
	st := &engine.State{Config: cfg, Runner: run}

	if applyTUI {
		// CleanOnFail must match the non-TUI construction below — hardcoding it here
		// meant --no-clean-on-failure was silently ignored under --tui.
		return tui.RunInit(context.Background(), title, st, phases.All(), !applyNoClean)
	}

	fmt.Println(ui.Title(title))
	if !write {
		fmt.Println(ui.Warn("plan — nothing is created or changed. `rackctl apply` provisions for real"))
	}
	eng := &engine.Engine{Phases: phases.All(), Out: os.Stdout, CleanOnFail: !applyNoClean}
	return eng.Run(context.Background(), st)
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
	// Both verbs take the same flags. plan ignores the two that only matter to a write, and
	// that is deliberate: an operator who plans with a flag and then applies without it should
	// not have the command reject the very line they just rehearsed.
	for _, c := range []*cobra.Command{planCmd, applyCmd} {
		f := c.Flags()
		f.StringVarP(&applyConfigPath, "config", "c", "rackctl.yaml", "path to rackctl.yaml")
		f.BoolVar(&applyNoClean, "no-clean-on-failure", false, "leave resources in place if a phase fails")
		f.BoolVar(&applyTUI, "tui", false, "interactive TUI progress view")
		f.BoolVar(&applySkipPreflight, "skip-preflight", false, "provision even if preflight says the install cannot succeed")
	}
}
