package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/rackctl/rackctl/internal/exec"
	"github.com/rackctl/rackctl/internal/ui"
)

// operatorOwnedKinds are the CRs whose controllers create resources in AWS that
// Terraform does not know about, and which are only cleaned up by the controller's
// own finalizer.
//
// Order matters: a Platform is deleted before the Tenant that owns it.
var operatorOwnedKinds = []string{
	"platforms.platform.nanohype.dev",
	"tenants.platform.nanohype.dev",
}

// reapOperatorOwnedResources deletes the CRs whose controllers own cloud resources
// and waits for their finalizers to run — BEFORE any terragrunt destroy removes the
// operator that would do the reaping.
//
// # WHY THIS EXISTS
//
// `rackctl destroy` walks the landing-zone components in reverse and never touches
// Kubernetes. That is fine for everything Terraform created, and wrong for everything
// the OPERATOR created: the eks-agent-platform Platform controller provisions IAM
// roles per Platform (a tenant role and a session role, both carrying the tenant
// baseline policy and permissions boundary), and those exist in no Terraform state.
//
// Destroying cluster-bootstrap/cluster kills the operator first. Its finalizers never
// run, the roles are orphaned, and the very next component — agent-iam — then fails:
//
//	Error: deleting IAM Policy (…/dev-eks-agent-platform-tenant-baseline):
//	  DeleteConflict: Cannot delete a policy attached to entities.
//
// The teardown halts partway, having already destroyed the cluster, leaving orphaned
// IAM roles and a half-torn-down account that needs manual repair. Observed exactly
// that way: dev-ops-tenant and dev-ops-session survived the cluster and blocked
// agent-iam.
//
// So: reap the CRs while the operator is still alive to honour their finalizers.
//
// Best-effort by design. A cluster that is already gone (or was never up) is not an
// error — there is nothing to reap and the components can be destroyed directly. But
// a finalizer that does NOT complete is reported loudly, because the consequence is
// silent orphaned IAM that bills and blocks.
func reapOperatorOwnedResources(ctx context.Context, run *exec.Runner) {
	if run.DryRun {
		fmt.Println(ui.Step("reap operator-owned resources (Platform/Tenant finalizers)"))
		for _, kind := range operatorOwnedKinds {
			fmt.Printf("    → (dry-run) kubectl delete %s --all -A --wait --timeout=5m\n", kind)
		}
		return
	}

	// No reachable cluster ⇒ nothing to reap. Not an error: destroy must still work
	// against a half-provisioned or already-deleted environment.
	if _, err := run.Capture(ctx, "kubectl", "get", "--raw", "/readyz"); err != nil {
		fmt.Println(ui.Step("no reachable cluster — skipping operator reap"))
		return
	}

	for _, kind := range operatorOwnedKinds {
		// A CRD that isn't installed is not a failure — the platform may simply not
		// have been deployed.
		if _, err := run.Capture(ctx, "kubectl", "get", "crd", kind); err != nil {
			continue
		}

		fmt.Println(ui.Step("reap " + kind))
		// --wait blocks on the finalizer, which is the entire point: we are waiting
		// for the operator to delete the IAM it created.
		out, err := run.Capture(ctx, "kubectl", "delete", kind,
			"--all", "--all-namespaces", "--wait", "--timeout=5m", "--ignore-not-found")
		if err != nil {
			// A finalizer that did not complete means the operator could not delete
			// its cloud resources. Terraform is about to fail on the policies still
			// attached to them, and the account will be left half-torn-down. Say so
			// plainly rather than letting it surface 3 components later as an opaque
			// DeleteConflict.
			fmt.Println(ui.Fail(fmt.Sprintf(
				"%s did not finalize — the operator could not delete the IAM roles it "+
					"created. agent-iam will fail on DeleteConflict. Check for orphaned "+
					"roles (aws iam list-roles --path-prefix /eks-agent-platform/) and any "+
					"<env>-<platform>-{tenant,session} roles before retrying.", kind)))
			continue
		}
		if s := strings.TrimSpace(out); s != "" {
			fmt.Println(ui.OK(s))
		}
	}
}
