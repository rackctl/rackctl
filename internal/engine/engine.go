package engine

import (
	"context"
	"fmt"
	"io"

	"github.com/rackctl/rackctl/internal/ui"
)

// Status is a phase's lifecycle state, reported via Engine.Hook.
type Status int

const (
	StatusStart Status = iota
	StatusOK
	StatusSkip
	StatusFail
)

// Event is emitted for each phase transition when Engine.Hook is set.
type Event struct {
	Index, Total int
	ID, Title    string
	Status       Status
	Err          error
}

// Engine runs an ordered pipeline of phases with cleanup-on-failure discipline
// (mirroring the always-teardown pattern in eks-agent-platform's `task e2e`).
type Engine struct {
	Phases      []Phase
	Out         io.Writer
	CleanOnFail bool
	Hook        func(Event) // if set, receives events and suppresses default printing
}

// Run executes each phase in order. On failure, completed phases are torn down
// in reverse so a half-failed init never strands billable resources.
func (e *Engine) Run(ctx context.Context, st *State) error {
	if st.Outputs == nil {
		st.Outputs = map[string]string{}
	}
	total := len(e.Phases)
	var completed []Phase
	for i, p := range e.Phases {
		ev := Event{Index: i + 1, Total: total, ID: p.ID(), Title: p.Title()}
		label := fmt.Sprintf("[%d/%d] %s", ev.Index, ev.Total, ev.Title)

		if p.Optional() && !p.Enabled(st) {
			ev.Status = StatusSkip
			e.report(ev, ui.Skip(label+"  (disabled)"))
			continue
		}
		ev.Status = StatusStart
		e.report(ev, ui.Step(label))

		if err := p.Run(ctx, st); err != nil {
			ev.Status, ev.Err = StatusFail, err
			e.report(ev, ui.Fail(label+"  — "+err.Error()))
			if e.CleanOnFail {
				e.teardown(ctx, st, completed)
			}
			return fmt.Errorf("phase %q failed: %w", p.ID(), err)
		}
		ev.Status = StatusOK
		e.report(ev, ui.OK(label))
		completed = append(completed, p)
	}
	if e.Hook == nil {
		fmt.Fprintln(e.Out, ui.OK("platform is up — hand off to the portal for day-2 operations"))
	}
	return nil
}

func (e *Engine) report(ev Event, line string) {
	if e.Hook != nil {
		e.Hook(ev)
		return
	}
	fmt.Fprintln(e.Out, line)
}

func (e *Engine) teardown(ctx context.Context, st *State, completed []Phase) {
	if e.Hook == nil {
		fmt.Fprintln(e.Out, ui.Warn("failure detected — rolling back provisioned resources"))
	}
	for i := len(completed) - 1; i >= 0; i-- {
		p := completed[i]
		if e.Hook == nil {
			fmt.Fprintln(e.Out, ui.Step("teardown: "+p.Title()))
		}
		if err := p.Teardown(ctx, st); err != nil && e.Hook == nil {
			fmt.Fprintln(e.Out, ui.Fail("teardown "+p.ID()+": "+err.Error()))
		}
	}
}
