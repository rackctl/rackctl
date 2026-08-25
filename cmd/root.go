// Package cmd wires the rackctl CLI.
package cmd

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "rackctl",
	Short: "The day-0 installer for a nanohype platform",
	Long: "rackctl provisions a full nanohype platform from zero — cloud, cluster,\n" +
		"GitOps, controllers, and portal — then hands off to the portal for day-2 ops.",
	SilenceUsage:  true,
	SilenceErrors: true,
}

// Execute runs the root command under a context cancelled by SIGINT or SIGTERM.
//
// The signal context is what makes an interrupt survivable. Every subprocess rackctl
// starts is an exec.CommandContext on a context descended from this one, so cancelling it
// terminates the in-flight terragrunt, kubectl or helm rather than leaving it parented to
// a dead shell. Without it the Go default disposition applies and the process dies at
// once — mid-apply, with whatever the child created since its last state write untracked
// by terraform and unreachable by a rollback that no longer has a process to run in.
//
// Commands reach it through cobra's cmd.Context(); none constructs its own root.
func Execute() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return rootCmd.ExecuteContext(ctx)
}

func init() {
	rootCmd.AddCommand(planCmd, applyCmd, destroyCmd, checkCmd, versionCmd)
}
