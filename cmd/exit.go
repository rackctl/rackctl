package cmd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/rackctl/rackctl/internal/engine"
	"github.com/rackctl/rackctl/internal/tui"
)

// Exit statuses. Every failure mode used to collapse to 1, which meant an agent — or a CI
// step, or a retry loop — could not tell "this config is invalid" from "the platform is
// half-built and billing". They are the same signal only if nothing downstream ever has to
// decide what to do next.
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
	// ExitStranded means a phase failed AND the rollback could not finish. This is the one
	// that needs a human: resources this run created may still exist. docs/runbook.md
	// covers what to look for.
	ExitStranded = 7
	// ExitAborted means the operator interrupted. Whether the unwind completed depends on
	// how far it got before a second interrupt.
	ExitAborted = 8
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
func classifyRun(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, tui.ErrAborted):
		return withExit(ExitAborted, err)
	case strings.Contains(err.Error(), "rollback did not complete"):
		return withExit(ExitStranded, err)
	case isLeftStanding(err):
		return withExit(ExitStanding, err)
	default:
		return withExit(ExitRolledBack, err)
	}
}

// isLeftStanding reports whether the engine declined to roll back. A NoRollbackError, an
// optional-phase failure and a run against a pre-existing platform all leave the cloud in
// place, which is the fact the caller needs.
func isLeftStanding(err error) bool {
	var noRollback *engine.NoRollbackError
	return errors.As(err, &noRollback) || strings.Contains(err.Error(), "optional phase(s) failed")
}
