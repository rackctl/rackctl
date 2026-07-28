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

// PlatformExists reports whether the platform was ALREADY provisioned when this run
// started. It is the difference between a rollback and a demolition.
//
// Overridable for tests; the default asks EKS.
var PlatformExists = func(ctx context.Context, st *State) bool {
	if st.Runner == nil || st.Config == nil || st.Runner.DryRun {
		return false
	}
	_, err := st.Runner.Capture(ctx, "aws", "eks", "describe-cluster",
		"--name", st.Config.ClusterName(),
		"--region", st.Config.Cloud.Region,
		"--query", "cluster.status", "--output", "text")
	return err == nil
}

// Run executes each phase in order. On failure, completed phases are torn down
// in reverse so a half-failed init never strands billable resources.
func (e *Engine) Run(ctx context.Context, st *State) error {
	if st.Outputs == nil {
		st.Outputs = map[string]string{}
	}

	// Rollback is only ever safe when this run BUILT the thing it is about to destroy.
	//
	// `init --apply` is re-runnable by design (#16): it is how an operator retries after a
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
	preexisting := PlatformExists(ctx, st)
	if preexisting && e.Hook == nil {
		fmt.Fprintln(e.Out, ui.Step("existing platform detected — a failure will NOT roll it back"))
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
			switch {
			case preexisting:
				// The platform was already up before this run touched it. Whatever just
				// failed, destroying it is not the remedy — this run did not build it.
				if e.Hook == nil {
					fmt.Fprintln(e.Out, ui.Warn(
						"the platform was already provisioned before this run — leaving it standing. "+
							"Nothing was rolled back. Run `rackctl doctor` to see what is wrong, or "+
							"`rackctl destroy --apply` to tear it down deliberately."))
				}
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
				if e.Hook == nil {
					fmt.Fprintln(e.Out, ui.Warn(
						"this is an optional phase and the platform underneath it is provisioned — "+
							"leaving it standing. Nothing was rolled back. Run `rackctl doctor` to see "+
							"what is wrong, or `rackctl destroy --apply` to tear it down deliberately."))
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
		reap.OperatorRoles(ctx, st.Runner, e.Out, st.KubeconfigCluster)
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
