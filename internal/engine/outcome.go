package engine

import (
	"errors"
	"fmt"
)

// Outcome is what a failed run left the cloud in. It is the fact a caller branches on, and
// it is decided where the decision is made: inside Run, by the arm that chose whether to
// roll back. Reading it back out of an error's text instead would make the classification
// depend on whatever a subprocess happened to print to stderr.
type Outcome int

const (
	// RolledBack means the rollback ran and every part of it held: what this run created
	// has been destroyed.
	RolledBack Outcome = iota + 1
	// Stranded means the rollback ran and could not finish, so resources this run created
	// may still exist.
	Stranded
	// LeftStanding means the run deliberately did not roll back over a platform that is
	// there to use: an optional phase failed, a phase declared its failure must not
	// destroy the cloud, or the platform was already standing when the run began. A
	// refusal before spend is Refused instead, even over a standing platform.
	LeftStanding
	// PlatformUnknownState means the run could not establish whether a platform already
	// existed, so it disabled the rollback. Nobody can vouch for what was left.
	PlatformUnknownState
	// CleanDisabled means the operator turned the rollback off with --no-clean-on-failure,
	// so whatever the run built before the failure is still there.
	CleanDisabled
	// Refused means a phase refused before anything was spent. Nothing was touched, so it
	// is recorded whatever the run found standing, including when it could not tell.
	Refused
)

// The reasons a run left the platform standing. They are carried for the message and for
// a caller that wants the detail; every one of them classifies the same way.
const (
	ReasonOptional    = "optional"
	ReasonNoRollback  = "no-rollback"
	ReasonPreexisting = "preexisting"
)

// RunError is the error Run returns for a failed run. Its message is the same text the
// operator always saw; its fields are what a caller reads with errors.As.
type RunError struct {
	// Phase is the phase that failed. It is empty for the end-of-run report of optional
	// phases that failed, where more than one may have.
	Phase   string
	Outcome Outcome
	// Reason says why a LeftStanding run was left standing.
	Reason string
	// DryRun is set on a plan. A plan creates nothing, so its outcome is what the rollback
	// WOULD have been, and a caller must not read it as a claim about the cloud.
	DryRun bool
	// Interrupted is set when the run's context was cancelled by the time it returned: the
	// operator sent an interrupt and the unwind, whatever it was, has finished.
	Interrupted bool
	// Err is the phase failure.
	Err error
	// TeardownErrs is what the rollback could not undo. Non-empty exactly when Outcome is
	// Stranded.
	TeardownErrs []error
}

func (e *RunError) Error() string {
	if e.Phase == "" {
		return fmt.Sprintf("optional phase(s) failed: %v", e.Err)
	}
	msg := fmt.Sprintf("phase %q failed: %v", e.Phase, e.Err)
	if len(e.TeardownErrs) > 0 {
		msg += fmt.Sprintf(" (and the rollback did not complete: %v)", errors.Join(e.TeardownErrs...))
	}
	return msg
}

// Unwrap exposes the phase failure and every teardown failure, so errors.Is and errors.As
// reach a NoRollbackError or a sentinel underneath either.
func (e *RunError) Unwrap() []error {
	return append([]error{e.Err}, e.TeardownErrs...)
}

// Refuse marks err as a refusal made before anything was spent: a pin that does not
// resolve, a live root that is missing, a tool or an identity the run cannot proceed
// without. It does not roll back, like any NoRollbackError, and it classifies as refused
// rather than as a platform left standing, because the run touched nothing. That holds on
// a re-apply over a standing platform and on a run that could not tell whether one was
// there. A nil error stays nil.
func Refuse(err error) error {
	if err == nil {
		return nil
	}
	return &NoRollbackError{Err: err, BeforeSpend: true}
}
