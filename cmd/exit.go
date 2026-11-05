package cmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/rackctl/rackctl/internal/engine"
	"github.com/rackctl/rackctl/internal/tui"
)

// Exit statuses. Collapsing every failure mode to 1 leaves an agent — or a CI step, or a
// retry loop — unable to tell "this config is invalid" from "the platform is half-built and
// billing". They are the same signal only if nothing downstream ever has to decide what to
// do next.
//
// The numbers are a contract: they are documented in docs/exit-codes.md, asserted by
// TestExitCodes_AreStableAndDistinct, and must not be renumbered.
const (
	// ExitOK is success.
	ExitOK = 0
	// ExitFailure is a failure with no more specific classification.
	ExitFailure = 1
	// ExitConfig means the config could not be loaded or did not validate. Nothing ran.
	ExitConfig = 2
	// ExitPreflight means preflight refused: this install would not succeed. Nothing was
	// created and nothing was spent.
	ExitPreflight = 3
	// ExitDeclined means a human refused, or there was no human to ask. Nothing was
	// destroyed.
	ExitDeclined = 4
	// ExitStanding means a phase failed and the platform was deliberately left standing —
	// an optional phase, a convergence timeout, or a run that did not build what it found.
	// Real resources exist and are billing; they are the ones that were already there or
	// that this run completed.
	ExitStanding = 5
	// ExitRolledBack means a phase failed and the rollback ran to completion. What this run
	// created has been destroyed.
	ExitRolledBack = 6
	// ExitStranded means a phase failed AND the rollback could not finish, or a teardown
	// ended with a backstop that guards billable or blocking resources failed or not run.
	// A second interrupt, which forces exit before the unwind finishes, exits this too.
	// This is the one that needs a human: resources this run created may still exist.
	// docs/runbook.md covers what to look for.
	ExitStranded = 7
	// ExitAborted means the operator interrupted, and whatever unwind the run owed has
	// finished. An interrupted run whose unwind did not finish exits ExitStranded instead:
	// the more urgent cloud state wins, and the message still says it was interrupted.
	ExitAborted = 8
	// ExitRefused means a command refused before it touched anything: an apply that refused
	// before any spend, or a destroy that refused before any destroy. Nothing changed.
	ExitRefused = 9
	// ExitUnhealthy means `rackctl check` ran and found the platform unhealthy, found
	// kubectl pointed at a different cluster, or found the cluster in EKS and could not
	// reach it from this shell.
	ExitUnhealthy = 10
)

// exitError carries an exit status alongside an error, so a command can classify its own
// failure without main having to pattern-match on message text.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

// withExit tags err with an exit status. A nil error stays nil.
func withExit(code int, err error) error {
	if err == nil {
		return nil
	}
	return &exitError{code: code, err: err}
}

// ExitCode classifies an error from Execute into a status main can exit with.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var e *exitError
	if errors.As(err, &e) {
		return e.code
	}
	return ExitFailure
}

// exitCodeName is used by the error message so an operator reading a terminal sees the same
// vocabulary an agent reads from the status.
func exitCodeName(code int) string {
	switch code {
	case ExitConfig:
		return "config"
	case ExitPreflight:
		return "preflight"
	case ExitDeclined:
		return "declined"
	case ExitStanding:
		return "platform-left-standing"
	case ExitRolledBack:
		return "rolled-back"
	case ExitStranded:
		return "resources-may-remain"
	case ExitAborted:
		return "aborted"
	case ExitRefused:
		return "refused"
	case ExitUnhealthy:
		return "unhealthy"
	default:
		return "failure"
	}
}

// Classify renders the status name for a failed run.
func Classify(err error) string {
	return fmt.Sprintf("%s (exit %d)", exitCodeName(ExitCode(err)), ExitCode(err))
}

// classifyRun turns an engine outcome into a status that says what state the cloud is in.
//
// The three cases an operator or an agent has to act on differently: the rollback ran and
// there is nothing to clean up, the platform was deliberately left standing and is usable,
// or the rollback could not finish and something may still be billing.
//
// It reads the outcome the engine recorded, never the message: that text carries
// subprocess stderr, and a tool that printed "rollback did not complete" would otherwise
// decide the status. An error that carries no outcome is not evidence of a rollback, so it
// stays the generic failure.
//
// ctx is the command's context. Its cancellation is how a plain-path interrupt arrives, so
// it is checked as well as the engine's own record of it.
func classifyRun(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	interrupted := ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, tui.ErrAborted)
	var run *engine.RunError
	if !errors.As(err, &run) {
		if interrupted {
			return withExit(ExitAborted, err)
		}
		return err
	}
	interrupted = interrupted || run.Interrupted
	code := runOutcomeCode(run)
	if interrupted {
		// An interrupt is aborted unless the unwind it triggered did not finish, in which
		// case what may still be billing is the more urgent fact.
		if code != ExitStranded {
			code = ExitAborted
		}
		return withExit(code, fmt.Errorf("interrupted: %w", err))
	}
	return withExit(code, err)
}

// runOutcomeCode maps a recorded outcome to its status. A plan built nothing, so whatever
// its rollback would have been, it is never a claim about the cloud.
func runOutcomeCode(run *engine.RunError) int {
	if run.DryRun {
		return ExitFailure
	}
	switch run.Outcome {
	case engine.RolledBack:
		return ExitRolledBack
	case engine.LeftStanding:
		return ExitStanding
	case engine.Refused:
		return ExitRefused
	case engine.Stranded, engine.PlatformUnknownState, engine.CleanDisabled:
		return ExitStranded
	default:
		return ExitFailure
	}
}
