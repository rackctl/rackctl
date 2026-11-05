// Package tui renders rackctl's interactive operator views.
package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/rackctl/rackctl/internal/engine"
	"github.com/rackctl/rackctl/internal/exec"
	"github.com/rackctl/rackctl/internal/ui"
)

// ErrAborted is the operator stopping the run from the TUI. It is deliberately an error:
// the run stopped part-way through, and a zero exit status would tell a script — or a
// person — that it succeeded.
var ErrAborted = errors.New(
	"run aborted by the operator — the platform is in an unknown state, run `rackctl check`")

// ErrForced is the second q: the operator forced exit before the unwind the first one
// started had finished. Every running tool was killed and nothing more was started, so
// what this run built may remain.
var ErrForced = fmt.Errorf("%w: forced exit before the unwind finished, so what this run built may "+
	"remain — run `rackctl destroy` to remove it", ErrAborted)

// abortGrace bounds how long a forced exit waits for the engine to return once every tool
// it started has been killed and no new one can start. It is a backstop against a phase
// that ignores cancellation, so the transcript is not closed while the engine still writes
// to it; the first q never applies it, because a rollback must run to completion.
const abortGrace = 30 * time.Second

// forceStop is exec.ForceStop, a variable so a test can force an exit without stopping
// every later test's tools in the same process.
var forceStop = exec.ForceStop

// stderr is where the transcript path and the engine's notices go once the view exits.
var stderr io.Writer = os.Stderr

type eventMsg engine.Event
type doneMsg struct{ err error }

// outcome is the engine's result, published by closing ready. A close reaches every reader,
// where a value sent on a channel reaches one: the view's own wait for it outlives a view
// that exits early and would otherwise take the value, leaving whoever then waits on the
// unwind waiting forever.
type outcome struct {
	ready chan struct{}
	err   error
}

func newOutcome() *outcome { return &outcome{ready: make(chan struct{})} }

// set records the engine's result and releases every reader. It is called once.
func (o *outcome) set(err error) {
	o.err = err
	close(o.ready)
}

type phaseRow struct {
	title  string
	status engine.Status
	active bool
	seen   bool
}

type model struct {
	title    string
	rows     []phaseRow
	events   chan engine.Event
	done     *outcome
	spinner  spinner.Model
	finished bool
	// aborted is the first q: the engine's context is cancelled and the view stays up while
	// the run unwinds, including a rollback, to its end.
	aborted bool
	// forced is the second q: exit now, abandoning whatever unwind is still running.
	forced bool
	// stop cancels the engine's context. Nil in tests that drive Update alone.
	stop context.CancelFunc
	// rollingBack is set once the engine reports a rollback has begun.
	rollingBack bool
	// noRollback is set when a failure would not roll back: the posture said so, or
	// --no-clean-on-failure was passed.
	noRollback bool
	// banner is the posture line, rendered under the title for the whole run.
	banner string
	// notices are the engine's operator lines, printed again after the view exits so they
	// survive the alternate screen.
	notices []string
	// success is the closing line of a run that finished.
	success string
	err     error
	// tail is the most recent line of subprocess output. A phase-level view of a
	// forty-minute apply is a spinner with nothing behind it; this is what the run is
	// waiting on, rendered under the active phase.
	tail func() string
}

// RunInit runs the bootstrap pipeline under an interactive TUI. Direct command
// output is silenced; the TUI renders phase-level status via the engine Hook.
//
// ctx governs the engine, not just the view. The first q cancels it, which stops the
// in-flight terragrunt the same way a first Ctrl-C does, and the view stays up while the run
// unwinds, a rollback included, to its end. The second q forces exit. The plain path does
// exactly the same, so the same keystroke leaves the cloud in the same state on both.
//
// Signals are not the view's to handle. Bubble Tea's own handler turns a SIGTERM or a
// SIGINT into an early return from the program, which ended the process while the engine,
// cancelled by root's forwarder, was still unwinding, and left its tools running in their
// own process groups. The program runs without it, so a signal reaches only root's
// forwarder, which treats it as it treats a q. A view that still returns before the engine
// has finished, for any reason, waits for the engine's outcome rather than reporting one it
// does not have.
// opts is for tests, which drive the program headless; production passes none.
func RunInit(ctx context.Context, title string, st *engine.State, ph []engine.Phase, cleanOnFail bool, opts ...tea.ProgramOption) error {
	// Subprocess output goes to a transcript rather than to io.Discard. The TUI owns the
	// terminal so this output cannot go to stdout, but discarding it leaves a failed run
	// with a cross beside a phase name and nothing else — and that output is the diagnosis.
	org := ""
	if st.Config != nil {
		org = st.Config.Org.Name
	}
	tr := newTranscript(transcriptDir(org), "apply")
	defer tr.Close()
	st.Runner.Out = tr

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	events := make(chan engine.Event, 128)
	done := newOutcome()
	eng := &engine.Engine{Phases: ph, Out: tr, CleanOnFail: cleanOnFail, Hook: func(ev engine.Event) { events <- ev }}
	go func() { done.set(eng.Run(ctx, st)) }()

	rows := make([]phaseRow, len(ph))
	for i, p := range ph {
		rows[i] = phaseRow{title: p.Title()}
	}
	sp := spinner.New()
	sp.Spinner = spinner.Dot

	// Read the outcome off the model bubbletea hands back, NOT off the local one.
	// Update has a value receiver, so every `m.err = …` it performs mutates the
	// program's copy; the local `m` is never touched. Returning it reported success
	// for every failed run. (setRow appears to work only because m.rows is a slice
	// sharing a backing array — which is exactly why this was easy to miss.)
	final, viewErr := tea.NewProgram(model{
		title: title, rows: rows, events: events, done: done, spinner: sp, stop: cancel,
		noRollback: !cleanOnFail, success: engine.SuccessLine(st.Config),
		tail: func() string { return tr.Tail(96) },
	}, append([]tea.ProgramOption{tea.WithoutSignalHandler()}, opts...)...).Run()
	fm, ok := final.(model)
	if viewErr == nil && !ok {
		viewErr = fmt.Errorf("tui: unexpected final model %T", final)
	}
	var err error
	switch {
	case ok && fm.forced:
		err = forceExit(events, done)
	case ok && fm.finished:
		err = fm.err
	default:
		// The view ended before the engine did. Its outcome is still the run's outcome, and
		// the process must outlive the unwind that produces it, so it is waited on as the
		// first q waits.
		why := "the view closed"
		if viewErr != nil {
			why = "the view failed (" + viewErr.Error() + ")"
		}
		fmt.Fprintf(stderr, "%s before the run finished; waiting for it to end, its rollback included. An "+
			"interrupt stops it, and a second one forces exit. Its output goes to %s\n", why, tr.Path())
		var notices []string
		notices, err = awaitEngine(events, done)
		fm.notices = append(fm.notices, notices...)
	}
	// The path and the engine's notices are reported after the view exits, so they survive
	// the alternate screen buffer, and on success as well as on failure — a run that worked
	// is also the one an operator comes back to when a problem surfaces later.
	tr.Report(stderr)
	for _, n := range fm.notices {
		fmt.Fprintln(stderr, n)
	}
	return err
}

// forceExit is the second q. Every tool the engine started is killed, process group and
// all, and no new one can start, so the engine returns promptly; it is waited on, bounded
// by abortGrace, so the transcript is not closed underneath it.
func forceExit(events chan engine.Event, done *outcome) error {
	forceStop()

	// The engine's Hook publishes to `events`, and with the view gone nothing is
	// draining it — a phase unwinding through more than the buffer would block in
	// the Hook forever and `done` would never fire. Keep draining until it does.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-events:
			case <-stop:
				return
			}
		}
	}()

	select {
	case <-done.ready:
		// The engine's error describes a run the operator abandoned; ErrForced says what
		// that left and what to run next, so it is what is returned.
	case <-time.After(abortGrace):
	}
	return ErrForced
}

// awaitEngine waits for the engine's outcome once the view is gone, draining the events its
// Hook publishes so the engine cannot block on a full channel, and keeping their notices.
// Unlike forceExit it is not bounded: the unwind it waits for must run to completion.
func awaitEngine(events chan engine.Event, done *outcome) ([]string, error) {
	var notices []string
	keep := func(ev engine.Event) {
		if ev.Notice != "" {
			notices = append(notices, ev.Notice)
		}
	}
	for {
		select {
		case ev := <-events:
			keep(ev)
		case <-done.ready:
			for {
				select {
				case ev := <-events:
					keep(ev)
				default:
					return notices, done.err
				}
			}
		}
	}
}

func waitEvent(ch chan engine.Event) tea.Cmd {
	return func() tea.Msg { return eventMsg(<-ch) }
}
func waitDone(o *outcome) tea.Cmd {
	return func() tea.Msg {
		<-o.ready
		return doneMsg{o.err}
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, waitEvent(m.events), waitDone(m.done))
}

func (m model) setRow(ev engine.Event) {
	if i := ev.Index - 1; i >= 0 && i < len(m.rows) {
		m.rows[i].status = ev.Status
		m.rows[i].seen = true
		m.rows[i].active = ev.Status == engine.StatusStart
	}
}

// record folds one engine event into the model.
func (m model) record(ev engine.Event) model {
	if ev.Notice != "" {
		m.notices = append(m.notices, ev.Notice)
		if ev.Posture {
			m.banner, m.noRollback = ev.Notice, true
		}
		if ev.Rollback {
			m.rollingBack = true
		}
		return m
	}
	m.setRow(ev)
	if ev.Err != nil && ev.Status == engine.StatusFail {
		m.err = ev.Err
	}
	return m
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	// A key PRESS, not the tea.KeyMsg interface. That interface is satisfied by key
	// releases as well, and on a terminal negotiating release reporting the same
	// keystroke would arrive twice — quitting once and then aborting a run that had
	// already finished quitting.
	case tea.KeyPressMsg:
		if s := msg.String(); s == "ctrl+c" || s == "q" {
			switch {
			case m.finished:
				// Quitting the summary after the run finished is just closing the view,
				// and must not turn a successful run into a non-zero exit.
				return m, tea.Quit
			case !m.aborted:
				// The first q stops the run and keeps the view up while it unwinds. Exiting
				// here would end the process partway through the rollback that follows,
				// with the EKS cluster and the VPC, last in that walk, never reached.
				m.aborted = true
				if m.stop != nil {
					m.stop()
				}
				return m, nil
			default:
				m.forced = true
				return m, tea.Quit
			}
		}
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case eventMsg:
		m = m.record(engine.Event(msg))
		return m, waitEvent(m.events)
	case doneMsg:
		// Drain any events still buffered before quitting.
		for drained := false; !drained; {
			select {
			case ev := <-m.events:
				m = m.record(ev)
			default:
				drained = true
			}
		}
		m.finished = true
		if msg.err != nil {
			m.err = msg.err
		}
		return m, tea.Quit
	}
	return m, nil
}

func (m model) View() tea.View {
	var b strings.Builder
	b.WriteString(ui.Bold.Render(m.title) + "\n\n")
	if m.banner != "" {
		b.WriteString(m.banner + "\n\n")
	}
	for _, r := range m.rows {
		var icon, suffix string
		switch {
		case r.active:
			icon = ui.Blue.Render(m.spinner.View())
		case !r.seen:
			icon = ui.Gray.Render("•")
		case r.status == engine.StatusOK:
			icon = ui.Green.Render("✓")
		case r.status == engine.StatusFail:
			icon = ui.Red.Render("✗")
		case r.status == engine.StatusTornDown:
			icon, suffix = ui.Yellow.Render("↺"), "  (torn down)"
		case r.status == engine.StatusTeardownFail:
			icon, suffix = ui.Red.Render("✗"), "  (teardown failed)"
		default: // skip
			icon, suffix = ui.Gray.Render("–"), "  (disabled)"
		}
		title := r.title + suffix
		if !r.seen && !r.active {
			title = ui.Gray.Render(title)
		}
		fmt.Fprintf(&b, " %s %s\n", icon, title)
	}
	b.WriteString("\n")
	switch {
	case m.finished && m.err != nil:
		b.WriteString(ui.Red.Render("✗ "+m.err.Error()) + "\n")
	case m.finished:
		b.WriteString(ui.Green.Render("✓ "+m.success) + "\n")
	default:
		if m.tail != nil {
			if s := m.tail(); s != "" {
				b.WriteString(ui.Gray.Render("  "+s) + "\n")
			}
		}
		b.WriteString(ui.Gray.Render("  "+m.footer()) + "\n")
	}
	return tea.NewView(b.String())
}

// footer says what q does now, before the operator presses it. A bare "q to quit" invites
// an abort mid-apply as though it were closing a window.
func (m model) footer() string {
	switch {
	case m.rollingBack:
		return "rolling back — q again forces exit and abandons the rollback, leaving what is not yet torn down"
	case m.aborted:
		return "stopping — the running tool is releasing its state; q again forces exit (resources may remain)"
	case m.noRollback:
		return "q stops the run and rolls nothing back (see above); q again forces exit"
	default:
		return "q stops the run and rolls back what it built; q again forces exit"
	}
}
