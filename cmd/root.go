// Package cmd wires the rackctl CLI.
package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	rackexec "github.com/rackctl/rackctl/internal/exec"
	"github.com/rackctl/rackctl/internal/phases"
	"github.com/rackctl/rackctl/internal/reap"
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
// stops the in-flight terragrunt, kubectl or helm rather than leaving it parented to a dead
// shell. A mutation is stopped gracefully: one SIGINT to its process group and up to
// exec.DefaultInterruptGrace to write state and release its lock (see internal/exec).
// Without the context the Go default disposition applies and the process dies at once,
// mid-apply, with whatever the child created since its last state write untracked by
// terraform and unreachable by a rollback that no longer has a process to run in.
//
// Commands reach it through cobra's cmd.Context(); none constructs its own root.
//
// A second interrupt must still be able to end the process, including during a rollback,
// which runs detached from this cancellation by design. Each mutating child runs in its own
// process group, so the terminal's second Ctrl-C no longer reaches it, and rackctl dying of
// the default disposition would leave tofu running with nothing to report to. So rackctl
// forwards it: forceExit kills every child's group, refuses to start another, and exits.
func Execute() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	go forwardInterrupts(sigs, cancel, forceExit)
	return rootCmd.ExecuteContext(ctx)
}

// forwardInterrupts cancels the run on the first signal and calls force on the second.
func forwardInterrupts(sigs <-chan os.Signal, cancel context.CancelFunc, force func()) {
	if _, ok := <-sigs; !ok {
		return
	}
	cancel()
	if _, ok := <-sigs; !ok {
		return
	}
	force()
}

// forceExit is the second interrupt. Whatever unwind the first one started has not
// finished, so what this run built may remain, and the status says so.
func forceExit() {
	rackexec.ForceStop()
	fmt.Fprintf(os.Stderr, "%s (exit %d): forced exit on a second interrupt — every running tool "+
		"was killed and the unwind did not finish, so what this run built may remain. Run `rackctl "+
		"check`, then `rackctl destroy` to remove it\n", exitCodeName(ExitStranded), ExitStranded)
	os.Exit(ExitStranded)
}

func init() {
	rootCmd.AddCommand(planCmd, applyCmd, destroyCmd, checkCmd, versionCmd)
	// The teardown paths report an unreachable cluster against this host's egress IP.
	reap.EgressIP = phases.EgressIP
}
