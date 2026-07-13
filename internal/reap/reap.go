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

// OrphanedVolumes deletes EBS volumes the cluster left behind, AFTER it is gone.
//
// Graceful PVC deletion is the right first move and an unreliable last one. A PVC
// carries a kubernetes.io/pvc-protection finalizer that blocks its deletion while any
// pod still mounts it, so `kubectl delete pvc --all --wait` hangs on exactly the
// workloads a teardown has not stopped yet — tempo, loki, the kagent database. It
// times out, the volumes are never released, and they outlive the cluster:
//
//	✗ PVCs did not delete cleanly — the EBS CSI driver may not have released their
//	  volumes.
//
// Unwinding that gracefully means stopping ArgoCD, pruning every workload, and only
// then deleting PVCs — a long sequence with several ways to stall, run against a
// cluster that is being demolished anyway.
//
// So this is the backstop, and it is deterministic. Every dynamically provisioned
// volume is tagged kubernetes.io/cluster/<name>=owned by the EBS CSI driver. Once the
// cluster is destroyed, no volume of its can still be attached — anything left with
// that tag in `available` state is an orphan, by definition. Delete it.
//
// Runs after the cluster is destroyed, never before: an in-use volume is skipped by
// the status filter, so this cannot detach a volume from anything living.
func OrphanedVolumes(ctx context.Context, run *exec.Runner, out io.Writer, cluster, region string) {
	if run.DryRun {
		fmt.Fprintln(out, ui.Step("sweep EBS volumes orphaned by "+cluster))
		return
	}

	ids, err := run.Capture(ctx, "aws", "ec2", "describe-volumes",
		"--region", region,
		"--filters",
		"Name=status,Values=available",
		"Name=tag-key,Values=kubernetes.io/cluster/"+cluster,
		"--query", "Volumes[].VolumeId", "--output", "text")
	if err != nil {
		return // no credentials, no region, nothing to do — not a teardown failure
	}
	ids = strings.TrimSpace(ids)
	if ids == "" || ids == "None" {
		return
	}

	vols := strings.Fields(ids)
	fmt.Fprintln(out, ui.Step(fmt.Sprintf("sweeping %d EBS volume(s) orphaned by %s", len(vols), cluster)))
	for _, v := range vols {
		if err := run.Run(ctx, "aws", "ec2", "delete-volume", "--region", region, "--volume-id", v); err != nil {
			fmt.Fprintln(out, ui.Fail("could not delete "+v+" — it will keep billing: "+err.Error()))
			continue
		}
		fmt.Fprintln(out, ui.OK("deleted "+v))
	}
}
