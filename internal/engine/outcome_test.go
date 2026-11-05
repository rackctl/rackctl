package engine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/exec"
)

// failPhase fails with err; its teardown fails with teardownErr.
type failPhase struct {
	recPhase
	err, teardownErr error
}

func (p failPhase) Run(context.Context, *State) error      { return p.err }
func (p failPhase) Teardown(context.Context, *State) error { return p.teardownErr }

func phase(id string, err error) failPhase {
	return failPhase{recPhase: recPhase{id: id, enabled: true, log: new([]string)}, err: err}
}

func runWith(t *testing.T, state PlatformState, cleanOnFail, dryRun bool, out io.Writer, ph ...Phase) *RunError {
	t.Helper()
	orig := PlatformExists
	t.Cleanup(func() { PlatformExists = orig })
	PlatformExists = func(context.Context, *State) PlatformState { return state }
	run := exec.New(io.Discard)
	run.DryRun = dryRun
	err := (&Engine{Phases: ph, Out: out, CleanOnFail: cleanOnFail}).Run(context.Background(),
		&State{Runner: run, Config: &config.Config{}})
	var re *RunError
	if !errors.As(err, &re) {
		t.Fatalf("a failed run must return a *RunError, got %T: %v", err, err)
	}
	return re
}

// Each arm that decides whether to roll back records what it left the cloud in. A caller
// classifies on that, so an arm that recorded nothing — or the wrong thing — would hand
// back a status the cloud does not match.
func TestRun_RecordsWhatEachFailureLeftTheCloudIn(t *testing.T) {
	boom := errors.New("boom")
	optional := phase("portal", boom)
	optional.optional = true
	for _, tc := range []struct {
		name        string
		state       PlatformState
		cleanOnFail bool
		phases      []Phase
		outcome     Outcome
		reason      string
	}{
		{"rolled back", PlatformAbsent, true, []Phase{phase("cluster", nil), phase("gitops", boom)}, RolledBack, ""},
		{"stranded", PlatformAbsent, true, []Phase{
			failPhase{recPhase: recPhase{id: "cluster", enabled: true, log: new([]string)}, teardownErr: errors.New("DependencyViolation")},
			phase("gitops", boom)}, Stranded, ""},
		{"optional", PlatformAbsent, true, []Phase{phase("cluster", nil), optional}, LeftStanding, ReasonOptional},
		{"no rollback", PlatformAbsent, true, []Phase{phase("gitops", &NoRollbackError{Err: boom})}, LeftStanding, ReasonNoRollback},
		{"preexisting", PlatformPresent, true, []Phase{phase("gitops", boom)}, LeftStanding, ReasonPreexisting},
		{"unknown", PlatformUnknown, true, []Phase{phase("gitops", boom)}, PlatformUnknownState, ""},
		{"clean disabled", PlatformAbsent, false, []Phase{phase("cluster", nil), phase("gitops", boom)}, CleanDisabled, ""},
		{"refused", PlatformAbsent, true, []Phase{phase("acquire", Refuse(boom))}, Refused, ""},
		// A refusal touched nothing, so it is a refusal whatever the run found standing. With
		// the platform state unknown it used to record that nobody could vouch for what was
		// left, over a run that left nothing.
		{"refused over a standing platform", PlatformPresent, true, []Phase{phase("acquire", Refuse(boom))}, Refused, ""},
		{"refused, platform state unknown", PlatformUnknown, true, []Phase{phase("acquire", Refuse(boom))}, Refused, ""},
		{"no rollback over a standing platform", PlatformPresent, true, []Phase{phase("gitops", &NoRollbackError{Err: boom})}, LeftStanding, ReasonPreexisting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			re := runWith(t, tc.state, tc.cleanOnFail, false, io.Discard, tc.phases...)
			if re.Outcome != tc.outcome || re.Reason != tc.reason {
				t.Errorf("outcome %d reason %q, want %d %q", re.Outcome, re.Reason, tc.outcome, tc.reason)
			}
			if re.DryRun || re.Interrupted {
				t.Errorf("DryRun=%v Interrupted=%v on a plain apply", re.DryRun, re.Interrupted)
			}
		})
	}
}

// The message an operator reads is unchanged: the phase, its failure, and what the rollback
// could not undo.
func TestRunError_KeepsTheOperatorsMessage(t *testing.T) {
	re := runWith(t, PlatformAbsent, true, false, io.Discard,
		failPhase{recPhase: recPhase{id: "cluster", enabled: true, log: new([]string)}, teardownErr: errors.New("DependencyViolation")},
		phase("substrate", errors.New("boom")))
	if want := `phase "substrate" failed: boom (and the rollback did not complete: teardown cluster: DependencyViolation)`; re.Error() != want {
		t.Errorf("message = %q, want %q", re.Error(), want)
	}
	optional := phase("portal", errors.New("boom"))
	optional.optional = true
	if re := runWith(t, PlatformAbsent, true, false, io.Discard, optional); re.Error() != "optional phase(s) failed: portal: boom" {
		t.Errorf("message = %q", re.Error())
	}
	var nr *NoRollbackError
	if re := runWith(t, PlatformAbsent, true, false, io.Discard, phase("x", Refuse(errors.New("pin")))); !errors.As(re, &nr) {
		t.Error("the phase failure must stay reachable through the RunError")
	}
	if Refuse(nil) != nil {
		t.Error("Refuse(nil) must stay nil")
	}
}

// A plan creates nothing, so its failure records that it was a plan, and its teardown says
// what an apply WOULD roll back rather than announcing a demolition.
func TestRun_APlanSaysItWouldRollBack(t *testing.T) {
	var out bytes.Buffer
	re := runWith(t, PlatformAbsent, true, true, &out, phase("cluster", nil), phase("gitops", errors.New("boom")))
	if !re.DryRun {
		t.Error("a failed plan must record that it was a plan")
	}
	if !strings.Contains(out.String(), "would roll back") || strings.Contains(out.String(), "rolling back provisioned") {
		t.Errorf("a plan's teardown must say it would roll back:\n%s", out.String())
	}
}

// An interrupt that cancelled the run is recorded once the unwind is over.
func TestRun_RecordsAnInterrupt(t *testing.T) {
	orig := PlatformExists
	t.Cleanup(func() { PlatformExists = orig })
	PlatformExists = func(context.Context, *State) PlatformState { return PlatformAbsent }
	ctx, cancel := context.WithCancel(context.Background())
	boom := ctxPhase{id: "boom", fail: true, cancelOnRun: cancel}
	err := (&Engine{Phases: []Phase{boom}, Out: io.Discard, CleanOnFail: true}).Run(ctx,
		&State{Runner: exec.New(io.Discard), Config: &config.Config{}})
	var re *RunError
	if !errors.As(err, &re) || !re.Interrupted || re.Outcome != RolledBack {
		t.Errorf("want an interrupted, rolled-back RunError, got %#v", err)
	}
}
