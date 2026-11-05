package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/rackctl/rackctl/internal/config"
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
	// StatusTornDown is a completed phase the rollback has undone.
	StatusTornDown
	// StatusTeardownFail is a phase the rollback could not undo.
	StatusTeardownFail
)

// Event is emitted for each phase transition when Engine.Hook is set.
//
// A notice event carries Notice and no phase: a line the operator must see whatever
// renders the rows, such as the rollback posture, a platform left standing, or the run's
// closing line. Every notice is also written to Engine.Out.
type Event struct {
	Index, Total int
	ID, Title    string
	Status       Status
	Err          error
	// Notice is a line for the operator. Index is zero on a notice.
	Notice string
	// Posture marks the notice that says whether a failure will roll back.
	Posture bool
	// Rollback marks the notice that a rollback has begun. Every teardown row follows it.
	Rollback bool
}

// Engine runs an ordered pipeline of phases with cleanup-on-failure discipline
// (mirroring the always-teardown pattern in eks-agent-platform's `task e2e`).
//
// Every line the run prints goes to Out, Hook or not: under the TUI, Out is the transcript,
// so the posture, the rollback and each teardown are on record there too. Hook decides only
// how the rows render.
type Engine struct {
	Phases      []Phase
	Out         io.Writer
	CleanOnFail bool
	Hook        func(Event) // if set, receives every phase transition and notice
}

// PlatformState answers "was the platform already standing when this run began?" —
// including the third case a bool could not express.
type PlatformState int

const (
	// PlatformAbsent means we asked and it is definitively not there.
	PlatformAbsent PlatformState = iota
	// PlatformPresent means we asked and it is definitively there.
	PlatformPresent
	// PlatformUnknown means the question could not be answered — expired
	// credentials, a throttle, no network, a denied permission.
	PlatformUnknown
)

// rollbackBlocked reports whether this state must suppress a rollback. Unknown
// counts, and that is the entire point of the type.
func (s PlatformState) rollbackBlocked() bool { return s != PlatformAbsent }

// PlatformExists reports whether the platform was ALREADY provisioned when this run
// started. It is the difference between a rollback and a demolition.
//
// It FAILS CLOSED. A `return err == nil` over a describe-cluster collapses "there is no
// platform" and "I could not find out" into the same answer — and picks the one that ARMS
// the rollback. Expired credentials midway
// through a long run, a throttle, or a transient network fault was enough to make a
// re-apply against a healthy platform look like a fresh install, and the teardown
// below destroys the EKS cluster and the VPC. The failure mode the whole rollback
// guard exists to prevent was reachable by the guard's own error path.
//
// list-clusters rather than describe-cluster because it separates the two answers
// without needing stderr: the call either succeeds — in which case membership is
// definitive either way — or it fails, which is the unknown.
//
// It asks on a plan too, through Query, which runs in a dry-run: whether a failed apply would
// roll back is the one thing about a rollback a plan can show for real, and a plan that
// assumed a fresh install would hide both "existing platform" and the fail-closed warning.
//
// Overridable for tests; the default asks EKS.
var PlatformExists = func(ctx context.Context, st *State) PlatformState {
	if st.Runner == nil || st.Config == nil {
		return PlatformAbsent
	}
	out, err := st.Runner.Query(ctx, "aws", "eks", "list-clusters",
		"--region", st.Config.Cloud.Region,
		"--query", fmt.Sprintf("contains(clusters, '%s')", st.Config.ClusterName()),
		"--output", "text")
	if err != nil {
		return PlatformUnknown
	}
	if strings.EqualFold(strings.TrimSpace(out), "true") {
		return PlatformPresent
	}
	return PlatformAbsent
}

// Run executes each phase in order. On failure, completed phases are torn down
// in reverse so a half-failed init never strands billable resources.
func (e *Engine) Run(ctx context.Context, st *State) error {
	if st.Outputs == nil {
		st.Outputs = map[string]string{}
	}

	// Rollback is only ever safe when this run BUILT the thing it is about to destroy.
	//
	// `rackctl apply` is re-runnable by design: it is how an operator retries after a
	// failure, and how they re-apply a config change to a platform that is already up.
	// Against an existing cluster, phases 1-4 all "succeed" as no-ops — the network is
	// there, the cluster is there, nothing is created — and are recorded as `completed`
	// all the same.
	//
	// A failure in any later phase would therefore tear those phases down, and phase 4's
	// teardown destroys the EKS cluster and the VPC. The blast radius is a healthy,
	// running platform this run did not create and was never asked to remove, reached
	// through the ordinary retry path rather than through anything exotic.
	//
	// NoRollbackError guards one case — a convergence timeout must not destroy the cloud.
	// This guards the other, and it is the more dangerous one: the operator did not lose a
	// wait, they lost the platform.
	//
	// So: if the cluster was already standing when the run began, never roll back. Report
	// the failure and leave everything exactly as it was. Tearing a platform down is
	// `rackctl destroy` — an explicit, separate act.
	state := PlatformExists(ctx, st)
	preexisting := state.rollbackBlocked()
	plan := st.Runner != nil && st.Runner.DryRun
	switch {
	case state == PlatformPresent && plan:
		e.posture(ui.Step("existing platform detected — plan only: apply would NOT roll back a failure over it"))
	case state == PlatformPresent:
		e.posture(ui.Step("existing platform detected — a failure will NOT roll it back"))
	case state == PlatformUnknown && plan:
		e.posture(ui.Warn(
			"could not determine whether a platform already exists — plan only: apply would NOT roll " +
				"back, because rollback is DISABLED for a run that cannot tell. Fix the credentials or " +
				"connectivity before applying."))
	case state == PlatformUnknown:
		// Say it out loud. The operator is the only one who can tell whether this
		// run is building from zero, and a silent fail-closed would leave a genuine
		// first install without the cleanup it expects.
		e.posture(ui.Warn(
			"could not determine whether a platform already exists — rollback is DISABLED " +
				"for this run. A failure will leave everything standing rather than risk " +
				"destroying a platform this run did not build. Fix the credentials or " +
				"connectivity and re-run to restore normal cleanup."))
	}

	total := len(e.Phases)
	// Indices into e.Phases of the phases that completed, so a teardown row is reported
	// against the phase it undoes.
	var completed []int
	// Optional phases that failed. Collected rather than returned, so one of them
	// failing does not cancel the others — see the Optional() case below.
	var optionalFailed []error
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
			// Optionality is decided FIRST, and the order is load-bearing. With
			// `case preexisting:` ahead of it this arm is shadowed entirely: on a re-apply
			// — which is when preexisting is true, and the ordinary way an operator retries
			// — a failing optional phase takes the preexisting branch, falls out of the
			// switch and returns. The
			// `continue` below was unreachable in exactly the situation it was written
			// for, so one optional phase failing still cancelled every optional phase
			// after it. Neither arm rolls back; only this one lets the run go on.
			//
			// Every arm that returns records what it left the cloud in, on fail; the caller
			// classifies on that rather than on the message's text.
			fail := &RunError{Phase: p.ID(), Err: err}
			switch {
			case p.Optional():
				// An OPTIONAL phase runs after the platform is already usable, and nothing
				// it installs is a prerequisite for anything before it. fleet installs a
				// control-plane factory, portal a day-2 UI, and smoke vends a first tenant
				// whose entire purpose is to PROVE the rest of the run worked.
				//
				// Rolling back because one of those failed destroys nine phases of
				// provisioned cloud to remedy a failure in the tenth. For smoke it is worse
				// than disproportionate: the check destroys the thing it was checking, so
				// the operator loses both the platform and the evidence of why it failed.
				//
				// All three end in a wait that can legitimately expire on a healthy
				// platform — Crossplane's provider install, a portal chart that is not
				// published yet and falls back to a local one, and a 15-minute wait on the
				// first tenant reaching Ready. That is the same reasoning NoRollbackError
				// encodes for the ArgoCD convergence wait, made structural here so the next
				// optional phase inherits it instead of having to remember.
				//
				// And the run CONTINUES. The reasoning above — that nothing an optional
				// phase installs is a prerequisite for anything else — cuts both ways.
				// Honouring only half of it, declining to roll back and then returning,
				// cancels every optional phase after the one that failed.
				//
				// What that cost is specific. portal is the day-2 UI and smoke is the
				// first-tenant vend, they share nothing, and portal is ordered first — so
				// a portal that could not install meant the tenant-vending path, the one
				// thing whose entire purpose is to prove the platform works, was never
				// exercised. The operator lost the evidence to a failure in the console.
				//
				// Each optional phase reads only what the CORE phases built, so skipping a
				// failed one leaves the next with everything it needs. The failures are
				// collected and reported together at the end, and the run still exits
				// non-zero: continuing is not forgiving them.
				optionalFailed = append(optionalFailed, fmt.Errorf("%s: %w", p.ID(), err))
				e.notice(ui.Warn(
					"this is an optional phase and the platform underneath it is provisioned — " +
						"leaving it standing. Nothing was rolled back, and the remaining phases " +
						"still run. Run `rackctl check` to see what is wrong, or `rackctl destroy` " +
						"to tear it down deliberately."))
				continue
			case errors.As(err, &noRollback) && noRollback.BeforeSpend:
				// A refusal before anything was spent left nothing at all, whatever was or was
				// not standing when the run began. It is decided ahead of the preexisting arm,
				// and the order is load-bearing: with the platform state unknown, a refusal
				// there would record that nobody can vouch for what was left, which reads as
				// something that may be billing over a run that touched nothing. An interrupt
				// that lands while the platform is being detected is the ordinary way to get
				// there: the detection fails, so the state is unknown, and preflight then
				// refuses on the cancelled context. Neither arm rolls back, so safety does not
				// rest on the order; only the recorded outcome does.
				fail.Outcome = Refused
			case preexisting:
				// The platform was already up before this run touched it — or we could not
				// establish that it wasn't. Whatever just failed, destroying it is not the
				// remedy: this run did not build it.
				//
				// The two are not the same outcome. A platform that was there is usable; one
				// nobody could establish either way may be half a fresh install, and nobody
				// can vouch for that.
				fail.Outcome, fail.Reason = LeftStanding, ReasonPreexisting
				if state == PlatformUnknown {
					fail.Outcome, fail.Reason = PlatformUnknownState, ""
				}
				switch {
				case plan && state == PlatformUnknown:
					e.notice(ui.Warn("plan only: whether a platform exists could not be told, so apply " +
						"would leave everything standing on this failure and would NOT roll back."))
				case plan:
					e.notice(ui.Warn("plan only: the platform was already provisioned, so apply would " +
						"leave it standing on this failure and would NOT roll back."))
				default:
					e.notice(ui.Warn(
						"the platform was already provisioned before this run — leaving it standing. " +
							"Nothing was rolled back. Run `rackctl check` to see what is wrong, or " +
							"`rackctl destroy` to tear it down deliberately."))
				}
			case errors.As(err, &noRollback):
				// Any NoRollbackError that is not a refusal before spend is a platform left up.
				fail.Outcome, fail.Reason = LeftStanding, ReasonNoRollback
			case !e.CleanOnFail:
				// --no-clean-on-failure. What this run built before the failure is still
				// there, and a half-built platform is not one anybody can call usable.
				fail.Outcome = CleanDisabled
				e.notice(ui.Warn(
					"--no-clean-on-failure: leaving what this run built in place. Nothing was rolled " +
						"back. `rackctl destroy` removes it."))
			default:
				// The phase that FAILED is torn down too. It failed partway, which
				// means it may have created resources before it died — a terragrunt
				// apply that errors on one resource has usually already created
				// several others. Rolling back only the phases that SUCCEEDED leaves
				// exactly those orphaned: a failed cluster-addons left seven IAM roles
				// behind, because its Teardown (which destroys the component) was
				// never called.
				fail.Outcome = RolledBack
				if tf := e.teardown(ctx, st, append(completed, i)); len(tf) > 0 {
					// A teardown that could not finish leaves billable resources standing,
					// which is the outcome the rollback exists to prevent — so it travels
					// with the phase failure rather than only being printed: under a Hook the
					// printed lines reach the transcript, not the screen, so the returned
					// error is what the operator reads.
					fail.Outcome, fail.TeardownErrs = Stranded, tf
				}
			}
			return e.finish(ctx, st, fail)
		}
		ev.Status = StatusOK
		e.report(ev, ui.OK(label))
		completed = append(completed, i)
	}
	// A platform whose optional layers failed is still a platform, and saying so is not
	// the same as calling the run a success. Both, in that order.
	if len(optionalFailed) > 0 {
		e.notice(ui.Warn(
			"the core platform is up; optional phases failed and are listed below. Nothing " +
				"was rolled back."))
		return e.finish(ctx, st, &RunError{Outcome: LeftStanding, Reason: ReasonOptional,
			Err: errors.Join(optionalFailed...)})
	}
	e.notice(ui.OK(SuccessLine(st.Config)))
	return nil
}

// SuccessLine is what a run that finished says. The portal is named only when this config
// installs one: an operator told to hand off to a portal that was never installed has
// nowhere to go.
func SuccessLine(cfg *config.Config) string {
	if cfg != nil && cfg.ControlPlane.Portal {
		return "platform is up — hand off to the portal for day-2 operations"
	}
	return "platform is up — `rackctl check` reports its health"
}

// finish stamps what is true of the whole run onto a failure: whether it was a plan, and
// whether an interrupt had cancelled it by the time the unwind was over.
func (e *Engine) finish(ctx context.Context, st *State, fail *RunError) error {
	fail.DryRun = st.Runner != nil && st.Runner.DryRun
	fail.Interrupted = ctx.Err() != nil
	return fail
}

// report writes a phase transition to Out and hands it to the Hook.
func (e *Engine) report(ev Event, line string) {
	e.print(line)
	if e.Hook != nil {
		e.Hook(ev)
	}
}

// notice writes a line the operator must see to Out and hands it to the Hook.
func (e *Engine) notice(line string) { e.emit(Event{Notice: line}) }

// posture is the notice that says whether a failure will roll back.
func (e *Engine) posture(line string) { e.emit(Event{Notice: line, Posture: true}) }

func (e *Engine) emit(ev Event) {
	e.print(ev.Notice)
	if e.Hook != nil {
		e.Hook(ev)
	}
}

func (e *Engine) print(line string) {
	if e.Out != nil {
		fmt.Fprintln(e.Out, line)
	}
}

// teardownBudget bounds a whole rollback. It is a reverse walk of the completed phases,
// each destroying one terragrunt component, plus the reap sweeps ahead of them. Half an
// hour is above that walk's honest worst case and below leaving the operator with no
// process and no answer.
// It must exceed exec.DefaultRunTimeout, and by more than one command's worth. At 30
// minutes it was HALF the ceiling a single terragrunt destroy is allowed, so one slow
// component could consume the whole budget and every phase after it would then run on an
// expired context — returning `context deadline exceeded` instantly and destroying
// nothing. The reverse walk puts the EKS cluster and the VPC last, so the phases silently
// skipped are exactly the ones holding the expensive resources the rollback exists to
// remove.
//
// Three hours: above a realistic worst case (an EKS control plane alone runs 15-25
// minutes, and substrate walks several components) with room for the per-command ceiling
// to be spent more than once, and still bounded rather than open-ended.
const teardownBudget = 3 * time.Hour

// teardown reverses the completed phases and returns whatever could not be undone.
//
// It runs on a context detached from the caller's cancellation, and that is the whole
// point rather than a convenience. A rollback is reached precisely when something went
// wrong, and one of the ways things go wrong is the operator interrupting: the TUI's
// abort path cancels the context, the in-flight phase's child dies, and this function is
// then handed the same cancelled context. Every command below is an exec.CommandContext
// on it, so each would return instantly — a rollback that reports having run and destroys
// nothing, while the abort's grace period makes it look like it was given time.
//
// Detaching keeps a ceiling rather than removing one. An operator who wants out during a
// rollback interrupts a second time, which takes the default signal disposition.
func (e *Engine) teardown(parent context.Context, st *State, completed []int) []error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), teardownBudget)
	defer cancel()

	// A plan provisioned nothing, so its teardown is the walk a rollback WOULD take, and
	// says so rather than announcing a demolition that is not happening.
	if st.Runner != nil && st.Runner.DryRun {
		e.emit(Event{Rollback: true, Notice: ui.Warn("failure detected — plan only: would roll back provisioned resources " +
			"(nothing is destroyed)")})
	} else {
		e.emit(Event{Rollback: true, Notice: ui.Warn("failure detected — rolling back provisioned resources")})
	}

	// Let the controllers delete what they — not Terraform — created, while they are
	// still alive to do it. Without this a rollback tears the cluster down on top of
	// live PVCs and Platform CRs, orphaning EBS volumes and IAM roles that nothing
	// will ever clean up — they are invisible to terraform state and bill indefinitely.
	// Both teardown paths need it, which is why it lives in its own package.
	//
	// But ONLY once this run actually built the cluster, and that condition is the whole
	// point rather than a tidy-up. Every call below acts on ambient state: reap.All and
	// reap.UnstickTerminating run `kubectl delete platforms|tenants|nodeclaims|pvc --all
	// -A` and patch finalizers off CRs against WHATEVER the kubeconfig currently points
	// at, and the cluster phase is the only place rackctl ever repoints it. So a failure
	// in preflight, acquire or identity — none of which create a cluster, and identity is
	// not wrapped in NoRollbackError — would otherwise reach this code with the kubeconfig
	// still aimed at the operator's previous context. Bootstrapping staging from a laptop
	// pointed at a healthy development cluster would then let a failed
	// `scripts/init-backend-aws.sh` delete every Platform and PVC in development.
	//
	// This is the same invariant assertComponentRoots holds with NoRollbackError, and the
	// reason it cannot be left to callers to remember: a precondition failure must never
	// be able to demolish something it exists to protect. NoRollbackError opts a single
	// error out; this makes the sweep structurally unreachable for every phase before the
	// cluster exists, including ones nobody has written yet.
	//
	// The component teardowns below are unaffected and still run for every completed
	// phase. They address terraform state, which is scoped by the state key rather than
	// by ambient context, so they can only ever destroy what this run created.
	if st.Runner != nil && e.Out != nil && st.KubeconfigCluster != "" {
		region := ""
		if st.Config != nil {
			region = st.Config.Cloud.Region
		}
		// A plan never repointed the operator's kubeconfig, so its in-cluster preview reads
		// through a temporary one aimed at the cluster by name, removed when the walk is done.
		// Without it the preview would read whatever the operator's context points at.
		pinned := true
		if st.Runner.DryRun {
			release, err := reap.PointAt(ctx, st.Runner, st.KubeconfigCluster, region)
			if err != nil {
				pinned = false
				e.print(ui.Warn("plan only: could not point a temporary kubeconfig at " + st.KubeconfigCluster +
					" (" + err.Error() + ") — the in-cluster preview was skipped"))
				st.Backstops = append(st.Backstops, reap.Backstop{Name: "in-cluster reap", Outcome: reap.NotRun,
					Detail: "(dry-run) no temporary kubeconfig for " + st.KubeconfigCluster + ": " + err.Error()})
			} else {
				defer release()
			}
		}
		if pinned {
			st.Backstops = append(st.Backstops, reap.All(ctx, st.Runner, e.Out, st.KubeconfigCluster, region))
		}

		// Force-delete the operator-minted IAM roles the finalizer may not have. A rollback
		// is the worst case for that finalizer — it runs against a half-built cluster where
		// the operator was very likely never healthy (its Pod Identity association is one of
		// the things that failed), so the roles almost certainly survived and would stop the
		// substrate teardown on agent-iam's DeleteConflict. Then free any CR the dead
		// finalizer left pinned in Terminating (safe once the roles are gone).
		org := ""
		if st.Config != nil {
			org = st.Config.Org.Name
		}
		st.Backstops = append(st.Backstops,
			reap.OperatorRoles(ctx, st.Runner, e.Out, reap.Owner{Org: org, Cluster: st.KubeconfigCluster}))
		if pinned {
			st.Backstops = append(st.Backstops, reap.UnstickTerminating(ctx, st.Runner, e.Out))
		}

		// And backstop the NodeClaim reap: a rollback runs against a half-built cluster,
		// which is exactly the case where Karpenter is not alive to honour a finalizer.
		// An instance that survives into the component teardown holds the node security
		// group, and Terraform cannot delete one that is in use — the rollback then
		// stops with the cluster gone and the instance still billing.
		if st.Config != nil {
			st.Backstops = append(st.Backstops,
				reap.OrphanedNodes(ctx, st.Runner, e.Out, st.Config.ClusterName(), st.Config.Cloud.Region))
		}
	} else {
		// Not a gap: with no cluster this run pointed kubectl at, these sweeps have nothing
		// they could safely act on. Said, so the summary accounts for every backstop.
		st.Backstops = append(st.Backstops, reap.Backstop{
			Name: "cluster sweeps (in-cluster, operator IAM roles, Karpenter nodes)", Outcome: reap.NotRun, Informational: true,
			Detail: "this run never pointed kubectl at a cluster it built, so there was nothing they could safely act on",
		})
	}
	// Every phase is attempted even after one fails: a component that cannot be destroyed
	// says nothing about the ones behind it, and stopping at the first would strand them.
	// Same posture as `rackctl destroy`.
	var failed []error
	for j := len(completed) - 1; j >= 0; j-- {
		i := completed[j]
		p := e.Phases[i]
		ev := Event{Index: i + 1, Total: len(e.Phases), ID: p.ID(), Title: p.Title()}
		e.print(ui.Step("teardown: " + p.Title()))
		if err := p.Teardown(ctx, st); err != nil {
			failed = append(failed, fmt.Errorf("teardown %s: %w", p.ID(), err))
			ev.Status, ev.Err = StatusTeardownFail, err
			e.report(ev, ui.Fail("teardown "+p.ID()+": "+err.Error()))
			continue
		}
		ev.Status = StatusTornDown
		e.report(ev, ui.OK("torn down: "+p.Title()))
	}
	// Which guards held, said once at the end. A guard that failed or did not run can leave a
	// Karpenter node billing or an operator role standing, which is a rollback that did not
	// complete exactly as a component that failed to destroy is, so it is returned with them
	// and the run classifies as resources-may-remain rather than rolled-back. Informational
	// backstops never reach that list.
	if e.Out != nil {
		reap.Summarize(e.Out, st.Backstops)
	}
	if unheld := reap.Unheld(st.Backstops); len(unheld) > 0 {
		e.notice(ui.Warn("not every backstop ran clean — resources outside terraform's state " +
			"may remain; the summary above names which"))
		failed = append(failed, fmt.Errorf("these backstops did not hold: %s", strings.Join(unheld, ", ")))
	}
	return failed
}
