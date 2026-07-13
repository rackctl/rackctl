// Package reap deletes the things a controller — not Terraform — created, while the
// controller is still alive to honour their finalizers.
//
// # THE RULE
//
// Any controller that creates cloud resources must be allowed to finalize before the
// infrastructure it runs on is torn down. Terraform does not know those resources
// exist, so nothing else will ever clean them up.
//
// Two instances, both found by tearing a real cluster down and watching what survived:
//
//   - The eks-agent-platform Platform controller mints IAM roles per Platform (a
//     tenant role and a session role, carrying the tenant baseline policy and
//     permissions boundary). Kill the operator first and the roles are orphaned —
//     agent-iam then fails with `DeleteConflict: Cannot delete a policy attached to
//     entities`, halting the teardown with the cluster already gone.
//
//   - The EBS CSI driver releases a dynamically provisioned volume when its PVC is
//     deleted. Kill the driver first and the volumes survive the cluster, attached to
//     nothing, billing.
//
// This lives in its own package because BOTH paths that tear a platform down need it:
// `rackctl destroy`, and the engine's rollback when an init fails partway. The
// rollback did not have it, and a failed install left three unattached gp3 volumes
// behind.
package reap

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/rackctl/rackctl/internal/exec"
	"github.com/rackctl/rackctl/internal/ui"
)

// operatorOwnedKinds are the CRs whose controllers create AWS resources Terraform does
// not track. Order matters: a Platform is deleted before the Tenant that owns it.
var operatorOwnedKinds = []string{
	"platforms.platform.nanohype.dev",
	"tenants.platform.nanohype.dev",
}

// All reaps everything a controller owns, in dependency order, and waits for the
// finalizers. It is best-effort by design: a cluster that is already gone, or was
// never provisioned, is not an error — there is simply nothing to reap.
//
// But a finalizer that does NOT complete is reported loudly, naming what to look for,
// because the alternative is that it surfaces several components later as an opaque
// DeleteConflict — after the cluster is already gone.
func All(ctx context.Context, run *exec.Runner, out io.Writer) {
	if run.DryRun {
		fmt.Fprintln(out, ui.Step("reap controller-owned resources (finalizers)"))
		for _, k := range operatorOwnedKinds {
			fmt.Fprintf(out, "    → (dry-run) kubectl delete %s --all -A --wait --timeout=5m\n", k)
		}
		fmt.Fprintf(out, "    → (dry-run) kubectl delete pvc --all -A --wait --timeout=5m\n")
		return
	}

	// No reachable cluster ⇒ nothing to reap. A teardown must still work against a
	// half-provisioned environment, which is exactly the case during a rollback.
	if _, err := run.Capture(ctx, "kubectl", "get", "--raw", "/readyz"); err != nil {
		fmt.Fprintln(out, ui.Step("no reachable cluster — nothing to reap"))
		return
	}

	for _, kind := range operatorOwnedKinds {
		// An uninstalled CRD is not a failure: the platform may never have been
		// deployed, which during a rollback is the common case.
		if _, err := run.Capture(ctx, "kubectl", "get", "crd", kind); err != nil {
			continue
		}
		fmt.Fprintln(out, ui.Step("reap "+kind))
		if _, err := run.Capture(ctx, "kubectl", "delete", kind,
			"--all", "--all-namespaces", "--wait", "--timeout=5m", "--ignore-not-found"); err != nil {
			fmt.Fprintln(out, ui.Fail(fmt.Sprintf(
				"%s did not finalize — the operator could not delete the IAM roles it created. "+
					"agent-iam will fail on DeleteConflict. Look for <env>-<platform>-{tenant,session} "+
					"roles and anything under: aws iam list-roles --path-prefix /eks-agent-platform/", kind)))
		}
	}

	// The CSI driver is an EKS addon and outlives ArgoCD, so this still works during a
	// rollback that has already torn the GitOps layer down.
	fmt.Fprintln(out, ui.Step("reap PersistentVolumeClaims (EBS CSI releases the volumes)"))
	out2, err := run.Capture(ctx, "kubectl", "delete", "pvc",
		"--all", "--all-namespaces", "--wait", "--timeout=5m", "--ignore-not-found")
	if err != nil {
		fmt.Fprintln(out, ui.Fail(
			"PVCs did not delete cleanly — the EBS CSI driver may not have released their volumes. "+
				"Check: aws ec2 describe-volumes --filters Name=status,Values=available"))
		return
	}
	if s := strings.TrimSpace(out2); s != "" {
		fmt.Fprintln(out, ui.OK(s))
	}
}
