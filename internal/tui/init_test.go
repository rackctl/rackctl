package tui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/rackctl/rackctl/internal/engine"
	"github.com/rackctl/rackctl/internal/exec"
)

// Update has a VALUE receiver, so everything it records lands on the model it
// returns and never on the receiver. Reading the outcome off a local copy instead
// misses every assignment Update made, so a failed run exits 0. Assert both halves:
// the returned model carries the error, and the receiver does not — the second is
// what makes the mistake invisible.
func TestUpdateRecordsErrorOnReturnedModelNotReceiver(t *testing.T) {
	boom := errors.New("phase 3 exploded")
	m := model{rows: []phaseRow{{title: "one"}}, events: make(chan engine.Event, 1)}

	next, _ := m.Update(doneMsg{err: boom})

	got, ok := next.(model)
	if !ok {
		t.Fatalf("Update returned %T, want model", next)
	}
	if !errors.Is(got.err, boom) {
		t.Errorf("returned model err = %v, want %v", got.err, boom)
	}
	if m.err != nil {
		t.Errorf("receiver err = %v, want nil — if this ever becomes non-nil the "+
			"value-receiver reasoning above no longer holds and RunInit should be revisited", m.err)
	}
}

// failingPhase is a stand-in pipeline step whose Run reports the given error.
type failingPhase struct{ err error }

func (failingPhase) ID() string                                    { return "boom" }
func (failingPhase) Title() string                                 { return "a phase that fails" }
func (failingPhase) Optional() bool                                { return false }
func (failingPhase) Enabled(*engine.State) bool                    { return true }
func (p failingPhase) Run(context.Context, *engine.State) error    { return p.err }
func (failingPhase) Teardown(context.Context, *engine.State) error { return nil }

// THE regression test. Returning the error off RunInit's own local model — which
// Update's value receiver can never write to — makes `rackctl apply --tui` exit 0
// on a genuine phase failure, in the invocation the quickstart, runbook and README
// all recommend. This fails against that shape.
func TestRunInitReturnsPhaseFailure(t *testing.T) {
	boom := errors.New("the cluster phase failed")
	st := &engine.State{Runner: exec.New(io.Discard)}

	err := RunInit(context.Background(), "test", st,
		[]engine.Phase{failingPhase{err: boom}}, false,
		tea.WithInput(nil), tea.WithoutRenderer())

	if err == nil {
		t.Fatal("RunInit returned nil for a failed phase — a failing apply would exit 0")
	}
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap %v", err, boom)
	}
}

// The mirror: a pipeline that succeeds must still report success.
func TestRunInitReturnsNilOnSuccess(t *testing.T) {
	st := &engine.State{Runner: exec.New(io.Discard)}

	err := RunInit(context.Background(), "test", st,
		[]engine.Phase{failingPhase{err: nil}}, false,
		tea.WithInput(nil), tea.WithoutRenderer())

	if err != nil {
		t.Errorf("RunInit returned %v for a clean run, want nil", err)
	}
}

// A phase failure arriving as an event must survive to the returned model too.
func TestUpdateRecordsPhaseEventError(t *testing.T) {
	boom := errors.New("cluster phase failed")
	m := model{rows: []phaseRow{{title: "one"}}, events: make(chan engine.Event, 1)}

	next, _ := m.Update(eventMsg(engine.Event{Index: 1, Status: engine.StatusFail, Err: boom}))

	if got := next.(model).err; !errors.Is(got, boom) {
		t.Errorf("err = %v, want %v", got, boom)
	}
}

// Quitting mid-run is an abort, not a clean exit: the pipeline is part-way through
// and the platform is left in whatever state the interrupted phase produced.
func TestQuitWhileRunningMarksAborted(t *testing.T) {
	for _, k := range []struct {
		name string
		msg  tea.KeyPressMsg
	}{
		{"q", tea.KeyPressMsg{Code: 'q', Text: "q"}},
		{"ctrl+c", tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}},
	} {
		t.Run(k.name, func(t *testing.T) {
			// The key is matched by its String(), so the construction above has to
			// produce the spelling Update compares against rather than merely being a
			// key of roughly the right shape.
			if got := k.msg.String(); got != k.name {
				t.Fatalf("key stringifies as %q, want %q — Update matches on this text, "+
					"so a test built on a different spelling asserts nothing", got, k.name)
			}
			m := model{rows: []phaseRow{{title: "one"}}}
			next, _ := m.Update(k.msg)
			if !next.(model).aborted {
				t.Errorf("quitting with %q mid-run did not set aborted", k.name)
			}
		})
	}
}

// Quitting the summary after the run finished is just closing the view. It must
// not turn a successful run into a non-zero exit.
func TestQuitAfterFinishIsNotAnAbort(t *testing.T) {
	m := model{rows: []phaseRow{{title: "one"}}, finished: true}
	next, _ := m.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if next.(model).aborted {
		t.Error("quitting after the run finished was treated as an abort")
	}
}

// The first q stops the run and keeps the view up while it unwinds; only the second exits.
// Exiting on the first ended the process partway through the rollback it had started, with
// the EKS cluster and the VPC, last in that walk, never reached.
func TestTheFirstQuitStopsTheRunAndOnlyTheSecondExits(t *testing.T) {
	stopped := false
	m := model{rows: []phaseRow{{title: "one"}}, stop: func() { stopped = true }}
	next, cmd := m.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	m = next.(model)
	if !stopped || !m.aborted {
		t.Fatal("the first q must cancel the engine's context")
	}
	if cmd != nil {
		if _, quit := cmd().(tea.QuitMsg); quit {
			t.Fatal("the first q exited the view — the rollback it starts would be cut off")
		}
	}
	if m.forced {
		t.Fatal("the first q is not a forced exit")
	}
	next, cmd = m.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	m = next.(model)
	if !m.forced || cmd == nil {
		t.Fatal("the second q must force exit")
	}
	if _, quit := cmd().(tea.QuitMsg); !quit {
		t.Fatal("the second q must quit the view")
	}
}

// slowTeardown is a completed phase whose teardown takes a while, and records that it
// finished.
type slowTeardown struct{ done *bool }

func (slowTeardown) ID() string                               { return "cluster" }
func (slowTeardown) Title() string                            { return "cluster" }
func (slowTeardown) Optional() bool                           { return false }
func (slowTeardown) Enabled(*engine.State) bool               { return true }
func (slowTeardown) Run(context.Context, *engine.State) error { return nil }
func (p slowTeardown) Teardown(context.Context, *engine.State) error {
	time.Sleep(200 * time.Millisecond)
	*p.done = true
	return nil
}

// stopsOnCancel is an in-flight phase that fails once the run is cancelled.
type stopsOnCancel struct{}

func (stopsOnCancel) ID() string                 { return "gitops" }
func (stopsOnCancel) Title() string              { return "gitops" }
func (stopsOnCancel) Optional() bool             { return false }
func (stopsOnCancel) Enabled(*engine.State) bool { return true }
func (stopsOnCancel) Run(ctx context.Context, _ *engine.State) error {
	<-ctx.Done()
	return ctx.Err()
}
func (stopsOnCancel) Teardown(context.Context, *engine.State) error { return nil }

// A q mid-run lets the rollback it triggers run to its end, and reports what the engine
// recorded, which is what decides the exit status, rather than a blanket abort.
func TestRunInit_AQuitLetsTheRollbackFinishAndReportsItsOutcome(t *testing.T) {
	orig := engine.PlatformExists
	t.Cleanup(func() { engine.PlatformExists = orig })
	engine.PlatformExists = func(context.Context, *engine.State) engine.PlatformState { return engine.PlatformAbsent }
	var notices bytes.Buffer
	stderr = &notices
	t.Cleanup(func() { stderr = os.Stderr })

	tornDown := false
	st := &engine.State{Runner: exec.New(io.Discard)}
	err := RunInit(context.Background(), "test", st,
		[]engine.Phase{slowTeardown{done: &tornDown}, stopsOnCancel{}}, true,
		tea.WithInput(strings.NewReader("q")), tea.WithoutRenderer())

	if !tornDown {
		t.Fatal("RunInit returned before the rollback finished")
	}
	var run *engine.RunError
	if !errors.As(err, &run) {
		t.Fatalf("the engine's outcome was dropped: %v", err)
	}
	if run.Outcome != engine.RolledBack || !run.Interrupted {
		t.Errorf("want a completed rollback of an interrupted run, got outcome=%v interrupted=%v", run.Outcome, run.Interrupted)
	}
	if !strings.Contains(notices.String(), "failure detected — rolling back provisioned resources") {
		t.Errorf("the rollback notice must be printed once the view exits:\n%s", notices.String())
	}
}

// The second q kills every tool and waits, bounded, for the engine to return. The engine's
// Hook publishes to a buffered channel that nothing drains once the view is gone, so the
// wait must keep draining it or an unwind that emits more than the buffer wedges.
func TestForceExit_StopsTheToolsAndDrainsTheEngine(t *testing.T) {
	stopped := false
	forceStop = func() { stopped = true }
	t.Cleanup(func() { forceStop = exec.ForceStop })
	events := make(chan engine.Event, 2) // deliberately smaller than what we emit
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 50; i++ { // would wedge on a full, undrained channel
			events <- engine.Event{Index: i}
		}
		done <- nil
	}()

	finished := make(chan error, 1)
	go func() { finished <- forceExit(events, done) }()
	select {
	case err := <-finished:
		if !errors.Is(err, ErrForced) || !errors.Is(err, ErrAborted) {
			t.Errorf("err = %v, want ErrForced", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("forceExit deadlocked — the engine's Hook blocked on an undrained events channel")
	}
	if !stopped {
		t.Error("a forced exit must kill the tools the engine started")
	}
}

// The view says what the run is doing to the cloud: the posture under the title, a
// disabled phase as disabled, a torn-down phase as torn down, and a footer that says what q
// does before anyone presses it.
func TestView_ShowsThePostureTheTeardownAndWhatQuitDoes(t *testing.T) {
	m := model{rows: []phaseRow{{title: "fleet"}, {title: "cluster"}}, events: make(chan engine.Event, 1),
		success: engine.SuccessLine(nil)}
	view := func() string { return m.View().Content }
	if !strings.Contains(view(), "q stops the run and rolls back what it built") {
		t.Errorf("the footer must say q rolls back:\n%s", view())
	}
	next, _ := m.Update(eventMsg(engine.Event{Notice: "existing platform detected — a failure will NOT roll it back", Posture: true}))
	m = next.(model)
	next, _ = m.Update(eventMsg(engine.Event{Index: 1, Status: engine.StatusSkip}))
	m = next.(model)
	next, _ = m.Update(eventMsg(engine.Event{Index: 2, Status: engine.StatusTornDown}))
	m = next.(model)
	for _, want := range []string{"existing platform detected", "fleet  (disabled)", "cluster  (torn down)",
		"rolls nothing back"} {
		if !strings.Contains(view(), want) {
			t.Errorf("missing %q:\n%s", want, view())
		}
	}
	next, _ = m.Update(eventMsg(engine.Event{Notice: "failure detected — rolling back", Rollback: true}))
	m = next.(model)
	if !strings.Contains(view(), "rolling back — q again forces exit") {
		t.Errorf("during a rollback the footer must say what the second q abandons:\n%s", view())
	}
	m.finished = true
	if strings.Contains(view(), "portal") {
		t.Errorf("no portal is installed, and the closing line hands off to one:\n%s", view())
	}
}
