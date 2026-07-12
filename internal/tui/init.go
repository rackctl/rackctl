// Package tui renders rackctl's interactive operator views.
package tui

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/rackctl/rackctl/internal/engine"
)

var (
	cGreen = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	cRed   = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	cGray  = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	cBlue  = lipgloss.NewStyle().Foreground(lipgloss.Color("4"))
	cBold  = lipgloss.NewStyle().Bold(true)
)

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
	err      error
}

// RunInit runs the bootstrap pipeline under an interactive TUI. Direct command
// output is silenced; the TUI renders phase-level status via the engine Hook.
func RunInit(title string, st *engine.State, ph []engine.Phase) error {
	st.Runner.Out = io.Discard

	events := make(chan engine.Event, 128)
	done := make(chan error, 1)
	eng := &engine.Engine{Phases: ph, Out: io.Discard, CleanOnFail: true, Hook: func(ev engine.Event) { events <- ev }}
	go func() { done <- eng.Run(context.Background(), st) }()

	rows := make([]phaseRow, len(ph))
	for i, p := range ph {
		rows[i] = phaseRow{title: p.Title()}
	}
	sp := spinner.New()
	sp.Spinner = spinner.Dot

	m := model{title: title, rows: rows, events: events, done: done, spinner: sp}
	if _, err := tea.NewProgram(m).Run(); err != nil {
		return err
	}
	return m.err
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
	b.WriteString(cBold.Render(m.title) + "\n\n")
	for _, r := range m.rows {
		var icon string
		switch {
		case r.active:
			icon = cBlue.Render(m.spinner.View())
		case !r.seen:
			icon = cGray.Render("•")
		case r.status == engine.StatusOK:
			icon = cGreen.Render("✓")
		case r.status == engine.StatusFail:
			icon = cRed.Render("✗")
		default: // skip
			icon = cGray.Render("•")
		}
		title := r.title
		if !r.seen && !r.active {
			title = cGray.Render(title)
		}
		b.WriteString(fmt.Sprintf(" %s %s\n", icon, title))
	}
	b.WriteString("\n")
	switch {
	case m.finished && m.err != nil:
		b.WriteString(cRed.Render("✗ "+m.err.Error()) + "\n")
	case m.finished:
		b.WriteString(cGreen.Render("✓ platform is up — hand off to the portal") + "\n")
	default:
		b.WriteString(cGray.Render("  q to quit") + "\n")
	}
	return b.String()
}
