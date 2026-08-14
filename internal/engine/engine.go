package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

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
// It FAILS CLOSED. This used to be `return err == nil` over a describe-cluster,
// which collapsed "there is no platform" and "I could not find out" into the same
// answer — and picked the one that ARMS the rollback. Expired credentials midway
// through a long run, a throttle, or a transient network fault was enough to make a
// re-apply against a healthy platform look like a fresh install, and the teardown
// below destroys the EKS cluster and the VPC. The failure mode the whole rollback
// guard exists to prevent was reachable by the guard's own error path.
//
// list-clusters rather than describe-cluster because it separates the two answers
// without needing stderr: the call either succeeds — in which case membership is
// definitive either way — or it fails, which is the unknown.
//
// Overridable for tests; the default asks EKS.
var PlatformExists = func(ctx context.Context, st *State) PlatformState {
	if st.Runner == nil || st.Config == nil || st.Runner.DryRun {
		return PlatformAbsent
	}
	out, err := st.Runner.Capture(ctx, "aws", "eks", "list-clusters",
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
	// `rackctl apply` is re-runnable by design (#16): it is how an operator retries after a
	// failure, and how they re-apply a config change to a platform that is already up.
	// Against an existing cluster, phases 1-4 all "succeed" as no-ops — the network is
	// there, the cluster is there, nothing is created. They are recorded as `completed`
	// all the same.
	//
	// So a failure in ANY later phase used to tear those phases down — and phase 4's
	// teardown destroys the EKS cluster and the VPC. A re-apply that tripped on a config
	// error would demolish a healthy, running platform that the run had not created and
	// was never asked to remove.
	//
	// That is not hypothetical. A re-apply failed on a ClusterRoleBinding conflict, the
	// engine began rolling back, and the only reason a 44/44-healthy cluster survived is
	// that the process happened to be killed mid-teardown.
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
	if e.Hook == nil {
		switch state {
		case PlatformPresent:
			fmt.Fprintln(e.Out, ui.Step("existing platform detected — a failure will NOT roll it back"))
		case PlatformUnknown:
			// Say it out loud. The operator is the only one who can tell whether this
			// run is building from zero, and a silent fail-closed would leave a genuine
			// first install without the cleanup it expects.
			fmt.Fprintln(e.Out, ui.Warn(
				"could not determine whether a platform already exists — rollback is DISABLED "+
					"for this run. A failure will leave everything standing rather than risk "+
					"destroying a platform this run did not build. Fix the credentials or "+
					"connectivity and re-run to restore normal cleanup."))
		}
	}

	total := len(e.Phases)
	var completed []Phase
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
			// Optionality is decided FIRST. `case preexisting:` used to come first and
			// shadowed this arm entirely: on a re-apply — which is when preexisting is
			// true, and the ordinary way an operator retries — a failing optional phase
			// took the preexisting branch, fell out of the switch and returned. The
			// `continue` below was unreachable in exactly the situation it was written
			// for, so one optional phase failing still cancelled every optional phase
			// after it. Neither arm rolls back; only this one lets the run go on.
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
				// phase installs is a prerequisite for anything else — cuts both ways, and
				// the loop used to honour only half of it: it declined to roll back, then
				// returned, so a failure in one optional phase cancelled every optional
				// phase after it.
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
				if e.Hook == nil {
					fmt.Fprintln(e.Out, ui.Warn(
						"this is an optional phase and the platform underneath it is provisioned — "+
							"leaving it standing. Nothing was rolled back, and the remaining phases "+
							"still run. Run `rackctl check` to see what is wrong, or `rackctl destroy` "+
							"to tear it down deliberately."))
				}
				continue
			case preexisting:
				// The platform was already up before this run touched it — or we could not
				// establish that it wasn't. Whatever just failed, destroying it is not the
				// remedy: this run did not build it.
				if e.Hook == nil {
					fmt.Fprintln(e.Out, ui.Warn(
						"the platform was already provisioned before this run — leaving it standing. "+
							"Nothing was rolled back. Run `rackctl check` to see what is wrong, or "+
							"`rackctl destroy` to tear it down deliberately."))
				}
			case e.CleanOnFail && !errors.As(err, &noRollback):
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
	// A platform whose optional layers failed is still a platform, and saying so is not
	// the same as calling the run a success. Both, in that order.
	if len(optionalFailed) > 0 {
		if e.Hook == nil {
			fmt.Fprintln(e.Out, ui.Warn(
				"the core platform is up; optional phases failed and are listed below. Nothing "+
					"was rolled back."))
		}
		return fmt.Errorf("optional phase(s) failed: %w", errors.Join(optionalFailed...))
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
	//
	// But ONLY once this run actually built the cluster, and that condition is the whole
	// point rather than a tidy-up. Every call below acts on ambient state: reap.All and
	// reap.UnstickTerminating run `kubectl delete platforms|tenants|nodeclaims|pvc --all
	// -A` and patch finalizers off CRs against WHATEVER the kubeconfig currently points
	// at, and the cluster phase is the only place rackctl ever repoints it. So a failure
	// in preflight, acquire or identity — none of which create a cluster, and identity is
	// not wrapped in NoRollbackError — used to reach this code with the kubeconfig still
	// aimed at the operator's previous context. Bootstrapping staging from a laptop
	// pointed at a healthy development cluster meant a failed `scripts/init-backend-aws.sh`
	// deleted every Platform and PVC in development.
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
		reap.All(ctx, st.Runner, e.Out)

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
		reap.OperatorRoles(ctx, st.Runner, e.Out, reap.Owner{Org: org, Cluster: st.KubeconfigCluster})
		reap.UnstickTerminating(ctx, st.Runner, e.Out)

		// And backstop the NodeClaim reap: a rollback runs against a half-built cluster,
		// which is exactly the case where Karpenter is not alive to honour a finalizer.
		// An instance that survives into the component teardown holds the node security
		// group, and Terraform cannot delete one that is in use — the rollback then
		// stops with the cluster gone and the instance still billing.
		if st.Config != nil {
			reap.OrphanedNodes(ctx, st.Runner, e.Out, st.Config.ClusterName(), st.Config.Cloud.Region)
		}
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
