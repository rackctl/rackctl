package tui

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rackctl/rackctl/internal/engine"
	"github.com/rackctl/rackctl/internal/exec"
)

// Update has a VALUE receiver, so everything it records lands on the model it
// returns and never on the receiver. RunInit used to read the outcome off its own
// local copy, which those assignments could not reach, so every failed run exited
// 0. Assert both halves: the returned model carries the error, and the receiver
// does not — the second is the part that made the bug invisible.
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

// THE regression test. RunInit used to return the error off its own local model,
// which Update's value receiver could never write to — so `rackctl apply --tui`
// exited 0 on a genuine phase failure, in the invocation the quickstart, runbook
// and README all recommend. This fails against that version and passes now.
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
	for _, key := range []string{"q", "ctrl+c"} {
		t.Run(key, func(t *testing.T) {
			m := model{rows: []phaseRow{{title: "one"}}}
			next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
			if key == "ctrl+c" {
				next, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
			}
			if !next.(model).aborted {
				t.Errorf("quitting with %q mid-run did not set aborted", key)
			}
		})
	}
}

// Quitting the summary after the run finished is just closing the view. It must
// not turn a successful run into a non-zero exit.
func TestQuitAfterFinishIsNotAnAbort(t *testing.T) {
	m := model{rows: []phaseRow{{title: "one"}}, finished: true}
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if next.(model).aborted {
		t.Error("quitting after the run finished was treated as an abort")
	}
}

// abort must cancel the engine's context and then WAIT for Run to return, so
// rackctl does not exit while terragrunt is still mid-apply.
func TestAbortCancelsAndWaitsForEngine(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan engine.Event, 4)
	done := make(chan error, 1)

	cancelled := make(chan struct{})
	go func() { // stand-in for engine.Run: unwinds only once cancelled
		<-ctx.Done()
		close(cancelled)
		done <- nil
	}()

	start := time.Now()
	err := abort(cancel, events, done)

	if !errors.Is(err, ErrAborted) {
		t.Errorf("err = %v, want ErrAborted", err)
	}
	select {
	case <-cancelled:
	default:
		t.Error("abort returned without the engine observing cancellation")
	}
	if time.Since(start) >= abortGrace {
		t.Error("abort fell through to the grace timeout instead of waiting on done")
	}
}

// The engine's Hook publishes to a buffered channel. Once the view is gone nothing
// drains it, so an unwind that emits more than the buffer would block in the Hook
// and `done` would never fire. abort must keep draining or it deadlocks.
func TestAbortDrainsEventsSoEngineCannotBlock(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan engine.Event, 2) // deliberately smaller than what we emit
	done := make(chan error, 1)

	go func() {
		<-ctx.Done()
		for i := 0; i < 50; i++ { // would wedge on a full, undrained channel
			events <- engine.Event{Index: i}
		}
		done <- nil
	}()

	finished := make(chan error, 1)
	go func() { finished <- abort(cancel, events, done) }()

	select {
	case err := <-finished:
		if !errors.Is(err, ErrAborted) {
			t.Errorf("err = %v, want ErrAborted", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("abort deadlocked — the engine's Hook blocked on an undrained events channel")
	}
}
