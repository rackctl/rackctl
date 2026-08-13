// Package tui renders rackctl's interactive operator views.
package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/rackctl/rackctl/internal/engine"
	"github.com/rackctl/rackctl/internal/ui"
)

// ErrAborted is returned when the operator quits the TUI while the pipeline is
// still running. It is deliberately an error: the run stopped part-way through,
// so the platform is in whatever state the interrupted phase left it in, and a
// zero exit status would tell a script — or a person — that it succeeded.
var ErrAborted = errors.New(
	"run aborted by the operator — the platform is in an unknown state, run `rackctl check`")

// abortGrace bounds how long we wait for the engine to unwind after a quit. The
// context cancellation kills its child processes, so this is a backstop against
// a phase that ignores cancellation, not the expected path.
const abortGrace = 30 * time.Second

type eventMsg engine.Event
type doneMsg struct{ err error }

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
	done     chan error
	spinner  spinner.Model
	finished bool
	aborted  bool
	err      error
}

// RunInit runs the bootstrap pipeline under an interactive TUI. Direct command
// output is silenced; the TUI renders phase-level status via the engine Hook.
//
// ctx governs the engine, not just the view: quitting the TUI cancels it, which
// is what terminates the in-flight terragrunt child rather than orphaning it.
// opts is for tests, which drive the program headless; production passes none.
func RunInit(ctx context.Context, title string, st *engine.State, ph []engine.Phase, cleanOnFail bool, opts ...tea.ProgramOption) error {
	st.Runner.Out = io.Discard

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	events := make(chan engine.Event, 128)
	done := make(chan error, 1)
	eng := &engine.Engine{Phases: ph, Out: io.Discard, CleanOnFail: cleanOnFail, Hook: func(ev engine.Event) { events <- ev }}
	go func() { done <- eng.Run(ctx, st) }()

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
	final, err := tea.NewProgram(model{
		title: title, rows: rows, events: events, done: done, spinner: sp,
	}, opts...).Run()
	if err != nil {
		return err
	}
	fm, ok := final.(model)
	if !ok {
		return fmt.Errorf("tui: unexpected final model %T", final)
	}
	if fm.aborted {
		return abort(cancel, events, done)
	}
	return fm.err
}

// abort unwinds the engine after the operator quits: cancel the context so the
// running child process dies, then wait for Run to return before we do. Without
// the wait, rackctl exits while terragrunt is mid-apply and whatever it created
// after its last state write is left untracked.
func abort(cancel context.CancelFunc, events chan engine.Event, done chan error) error {
	cancel()

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
	case <-done:
	case <-time.After(abortGrace):
	}
	return ErrAborted
}

func waitEvent(ch chan engine.Event) tea.Cmd {
	return func() tea.Msg { return eventMsg(<-ch) }
}
func waitDone(ch chan error) tea.Cmd {
	return func() tea.Msg { return doneMsg{<-ch} }
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

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if s := msg.String(); s == "ctrl+c" || s == "q" {
			// Only an abort if the pipeline is still running. Quitting the summary
			// after it finished is just closing the view, and must not turn a
			// successful run into a non-zero exit.
			if !m.finished {
				m.aborted = true
			}
			return m, tea.Quit
		}
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case eventMsg:
		ev := engine.Event(msg)
		m.setRow(ev)
		if ev.Err != nil {
			m.err = ev.Err
		}
		return m, waitEvent(m.events)
	case doneMsg:
		// Drain any events still buffered before quitting.
		for drained := false; !drained; {
			select {
			case ev := <-m.events:
				m.setRow(ev)
				if ev.Err != nil {
					m.err = ev.Err
				}
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

func (m model) View() string {
	var b strings.Builder
	b.WriteString(ui.Bold.Render(m.title) + "\n\n")
	for _, r := range m.rows {
		var icon string
		switch {
		case r.active:
			icon = ui.Blue.Render(m.spinner.View())
		case !r.seen:
			icon = ui.Gray.Render("•")
		case r.status == engine.StatusOK:
			icon = ui.Green.Render("✓")
		case r.status == engine.StatusFail:
			icon = ui.Red.Render("✗")
		default: // skip
			icon = ui.Gray.Render("•")
		}
		title := r.title
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
		b.WriteString(ui.Green.Render("✓ platform is up — hand off to the portal") + "\n")
	default:
		// Say what quitting costs. The old "q to quit" invited an abort mid-apply
		// as though it were closing a window.
		b.WriteString(ui.Gray.Render("  q aborts the run — the platform is left part-provisioned") + "\n")
	}
	return b.String()
}
