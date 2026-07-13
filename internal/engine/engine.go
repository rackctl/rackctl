package engine

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/rackctl/rackctl/internal/reap"
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
			// A phase can declare that its failure must not tear the platform down —
			// see NoRollbackError. A workload that has not converged is not a reason to
			// destroy the cloud it is running on.
			var noRollback *NoRollbackError
			if e.CleanOnFail && !errors.As(err, &noRollback) {
				// The phase that FAILED is torn down too. It failed partway, which
				// means it may have created resources before it died — a terragrunt
				// apply that errors on one resource has usually already created
				// several others. Rolling back only the phases that SUCCEEDED leaves
				// exactly those orphaned: a failed cluster-addons left seven IAM roles
				// behind, because its Teardown (which destroys the component) was
				// never called.
				e.teardown(ctx, st, append(completed, p))
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

	// Let the controllers delete what they — not Terraform — created, while they are
	// still alive to do it. Without this a rollback tears the cluster down on top of
	// live PVCs and Platform CRs, orphaning EBS volumes and IAM roles that nothing
	// will ever clean up. `rackctl destroy` already did this; the rollback did not,
	// and a failed install left three unattached volumes behind.
	if st.Runner != nil && e.Out != nil {
		reap.All(ctx, st.Runner, e.Out)
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
