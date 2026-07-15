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
// not track. Order matters:
//
//   - A Platform is deleted before the Tenant that owns it.
//   - NodeClaims come LAST. Karpenter's nodes are where everything else still runs, so
//     tearing them out from under the operator would prevent the finalizers above from
//     ever completing.
//
// NodeClaims are the third instance of this package's rule, and the one that cost the
// most to find. Karpenter creates EC2 instances; Terraform does not know they exist.
// Destroy the cluster and Karpenter dies with it, leaving its nodes running — orphaned,
// attached to nothing, and billing.
//
// That used to be invisible, because Karpenter's nodes sat in the EKS-managed CLUSTER
// security group, which EKS deletes along with the cluster. Once they were moved into
// the Terraform-managed NODE security group (which is what lets Cilium's rules cover
// them), the orphan became load-bearing: Terraform cannot delete a security group that
// an instance still holds, so the whole teardown stopped dead —
//
//	Error: deleting Security Group (sg-...): DependencyViolation
//
// — with the cluster already gone and an m9g still running. The fix is not to move the
// nodes back; it is to reap what a controller made, before killing the controller.
var operatorOwnedKinds = []string{
	"platforms.platform.nanohype.dev",
	"tenants.platform.nanohype.dev",
	"nodeclaims.karpenter.sh",
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

// execer is the slice of *exec.Runner that reap's deterministic backstops use. Naming it
// here is what lets those backstops be unit-tested with a fake — no live cloud, no cluster.
// *exec.Runner satisfies it; DryRun is passed alongside because it is a field, not a method.
type execer interface {
	Run(ctx context.Context, name string, args ...string) error
	Capture(ctx context.Context, name string, args ...string) (string, error)
}

// OperatorRoles force-deletes the IAM roles the eks-agent-platform operator mints per
// Platform. It is the deterministic backstop to the Platform-finalizer reap in All(), and
// it MUST run before agent-iam is destroyed.
//
// The operator's finalizer already deletes these roles itself, synchronously, before it
// drops the finalizer — so after a HEALTHY teardown All()'s `kubectl delete platforms
// --wait` has left nothing here to do. But a teardown is frequently run against an operator
// that is not healthy: crashlooping, already torn down, or — the case that actually bit —
// running on the EC2 node role because its Pod Identity association never existed, so every
// IAM call it makes is 403. Then the finalizer never completes, `--wait` times out, and the
// roles survive, still attached to the tenant baseline policy that agent-iam is about to
// destroy. terraform then stops the entire teardown dead:
//
//	Error: deleting IAM Policy (...): DeleteConflict: Cannot delete a policy attached to entities
//
// — with the cluster already half gone. That was cleared by hand, the same way, three times
// across two days of fresh installs.
//
// This is the fix, and it is also why an interrupted teardown no longer wedges (the second
// half of the same gap): the teardown's success no longer DEPENDS on the operator's
// finalizer completing. Whether the finalizer ran or not, the roles are gone before
// agent-iam runs. It reaps them exactly as the operator would — detach managed policies,
// delete inline policies, delete the role — but does not need the operator, or even a
// reachable cluster, to do it. IAM is global, so no region is needed either.
//
// Enumeration is by IAM path: the operator mints every tenant and session role under
// /eks-agent-platform/ (its default TenantIAMPath, which the catalog does not override).
// An org that repoints TenantIAMPath elsewhere would have to widen this prefix to match.
func OperatorRoles(ctx context.Context, run *exec.Runner, out io.Writer) {
	reapOperatorRoles(ctx, run, run.DryRun, out)
}

func reapOperatorRoles(ctx context.Context, run execer, dryRun bool, out io.Writer) {
	const pathPrefix = "/eks-agent-platform/"
	if dryRun {
		fmt.Fprintln(out, ui.Step("force-delete operator-minted IAM roles under "+pathPrefix+" (agent-iam DeleteConflict backstop)"))
		return
	}

	// --path-prefix is a server-side filter and the CLI auto-paginates, so this returns
	// every matching role regardless of how many Platforms existed.
	names, err := run.Capture(ctx, "aws", "iam", "list-roles",
		"--path-prefix", pathPrefix,
		"--query", "Roles[].RoleName", "--output", "text")
	if err != nil {
		return // no credentials, or IAM unreachable — not a teardown failure
	}
	roles := strings.Fields(strings.TrimSpace(names))
	if len(roles) == 0 {
		return
	}

	fmt.Fprintln(out, ui.Step(fmt.Sprintf(
		"force-deleting %d operator-minted IAM role(s) a finalizer left behind (agent-iam would else fail on DeleteConflict)", len(roles))))
	for _, name := range roles {
		if err := forceDeleteRole(ctx, run, name); err != nil {
			fmt.Fprintln(out, ui.Fail(
				"could not delete "+name+" — agent-iam may still fail on DeleteConflict: "+err.Error()))
			continue
		}
		fmt.Fprintln(out, ui.OK("deleted "+name))
	}
}

// forceDeleteRole detaches everything from a role and deletes it, mirroring the operator's
// own detachAndDeleteRole so the end state is identical to a clean finalizer run. Order is
// load-bearing: IAM refuses to delete a role that still has policies attached, exactly as it
// refuses to delete a policy still attached to a role — detach first, delete last.
func forceDeleteRole(ctx context.Context, run execer, name string) error {
	attached, err := run.Capture(ctx, "aws", "iam", "list-attached-role-policies",
		"--role-name", name, "--query", "AttachedPolicies[].PolicyArn", "--output", "text")
	if err != nil {
		return err
	}
	for _, arn := range strings.Fields(strings.TrimSpace(attached)) {
		if err := run.Run(ctx, "aws", "iam", "detach-role-policy",
			"--role-name", name, "--policy-arn", arn); err != nil {
			return err
		}
	}

	inline, err := run.Capture(ctx, "aws", "iam", "list-role-policies",
		"--role-name", name, "--query", "PolicyNames[]", "--output", "text")
	if err != nil {
		return err
	}
	for _, pol := range strings.Fields(strings.TrimSpace(inline)) {
		if err := run.Run(ctx, "aws", "iam", "delete-role-policy",
			"--role-name", name, "--policy-name", pol); err != nil {
			return err
		}
	}

	return run.Run(ctx, "aws", "iam", "delete-role", "--role-name", name)
}

// UnstickTerminating force-removes the finalizers from any Platform or Tenant still pinned in
// Terminating, so an interrupted or half-finalized teardown does not leave the cluster
// wedged. It is the companion to OperatorRoles and MUST run after it.
//
// A Platform blocks its own deletion with a finalizer that deletes the tenant namespace,
// revokes the KMS grant, trims the artifacts-bucket policy, and deletes the IAM roles. When
// the operator cannot finish that — it is crashlooping, or was itself already pruned by the
// cluster-bootstrap teardown — the CR is pinned in Terminating with nothing left alive to
// finalize it. On a re-run or a `doctor` afterward that reads as an unrecoverable wedge.
//
// In a teardown the finalizer is by then guarding nothing: the namespace and KMS grant die
// with the cluster, the buckets and roles are destroyed by terraform, and OperatorRoles has
// already force-deleted the roles. So removing it is safe — and ONLY here. This is called
// exclusively from the two teardown paths; run anywhere else, dropping a finalizer would
// orphan live AWS state, which is the precise failure this whole package exists to prevent.
//
// Best-effort and cluster-optional: a teardown against an unreachable cluster has no CRs to
// unstick, which is not an error.
func UnstickTerminating(ctx context.Context, run *exec.Runner, out io.Writer) {
	unstickTerminating(ctx, run, run.DryRun, out)
}

func unstickTerminating(ctx context.Context, run execer, dryRun bool, out io.Writer) {
	// Platform is namespaced, Tenant is cluster-scoped — the jsonpath emits the namespace
	// (empty for Tenant) so the patch can add -n only when there is one.
	kinds := []string{"platforms.platform.nanohype.dev", "tenants.platform.nanohype.dev"}
	if dryRun {
		for _, k := range kinds {
			fmt.Fprintln(out, ui.Step("(dry-run) unstick any "+k+" pinned in Terminating (finalizer force-removed)"))
		}
		return
	}
	if _, err := run.Capture(ctx, "kubectl", "get", "--raw", "/readyz"); err != nil {
		return // no reachable cluster — nothing to unstick
	}

	for _, kind := range kinds {
		if _, err := run.Capture(ctx, "kubectl", "get", "crd", kind); err != nil {
			continue // CRD never installed — this platform did not deploy the operator
		}
		// Emit "<namespace> <name>" for every instance carrying a deletionTimestamp, i.e.
		// every one stuck Terminating. A cluster-scoped Tenant emits a leading space.
		listed, err := run.Capture(ctx, "kubectl", "get", kind, "--all-namespaces",
			"-o", `jsonpath={range .items[?(@.metadata.deletionTimestamp)]}{.metadata.namespace}{" "}{.metadata.name}{"\n"}{end}`)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(strings.TrimSpace(listed), "\n") {
			ns, name, found := strings.Cut(strings.TrimSpace(line), " ")
			if !found { // cluster-scoped: no namespace, the whole line is the name
				ns, name = "", strings.TrimSpace(line)
			}
			if name == "" {
				continue
			}
			fmt.Fprintln(out, ui.Step("unsticking "+kind+" "+strings.TrimPrefix(ns+"/"+name, "/")+
				" (finalizer removed; its AWS state is already reaped)"))
			args := []string{}
			if ns != "" {
				args = append(args, "-n", ns)
			}
			args = append(args, "patch", kind, name, "--type=json",
				"-p", `[{"op":"remove","path":"/metadata/finalizers"}]`)
			if err := run.Run(ctx, "kubectl", args...); err != nil {
				fmt.Fprintln(out, ui.Fail("could not unstick "+name+": "+err.Error()))
			}
		}
	}
}

// OrphanedNodes terminates Karpenter's EC2 instances, and must run BEFORE the cluster
// component is destroyed — unlike OrphanedVolumes, which is a post-destroy sweep.
//
// It is the backstop for the NodeClaim reap in All(). That reap is the right first move
// and cannot be relied on as the last: it needs a reachable cluster and a live Karpenter
// to honour the finalizers, and a teardown is frequently run against a cluster that is
// neither — a failed install, an interrupted rollback, an operator that cannot reach the
// IAM API to finalize.
//
// When it does not run, the consequence is not cosmetic. Karpenter's nodes sit in the
// Terraform-managed NODE security group, and Terraform cannot delete a security group an
// instance still holds:
//
//	Error: deleting Security Group (sg-...): DependencyViolation
//
// The teardown stops there, with the EKS cluster already destroyed, the VPC still up,
// and an instance still billing — the worst possible place to stop, because the thing
// that could have cleaned up is the thing that was just deleted.
//
// The filter is exact rather than heuristic: Karpenter stamps every instance it launches
// with karpenter.sh/managed-by=<cluster-name>. No instance carrying that tag belongs to
// anything else, so this cannot touch a node the operator did not ask for.
func OrphanedNodes(ctx context.Context, run *exec.Runner, out io.Writer, cluster, region string) {
	if run.DryRun {
		fmt.Fprintln(out, ui.Step("terminate EC2 instances Karpenter launched for "+cluster))
		return
	}

	ids, err := run.Capture(ctx, "aws", "ec2", "describe-instances",
		"--region", region,
		"--filters",
		"Name=instance-state-name,Values=running,pending,stopping,stopped",
		"Name=tag:karpenter.sh/managed-by,Values="+cluster,
		"--query", "Reservations[].Instances[].InstanceId", "--output", "text")
	if err != nil {
		return // no credentials, no region — not a teardown failure
	}
	ids = strings.TrimSpace(ids)
	if ids == "" || ids == "None" {
		return
	}

	insts := strings.Fields(ids)
	fmt.Fprintln(out, ui.Step(fmt.Sprintf("terminating %d Karpenter instance(s) left by %s", len(insts), cluster)))
	args := append([]string{"ec2", "terminate-instances", "--region", region, "--instance-ids"}, insts...)
	if err := run.Run(ctx, "aws", args...); err != nil {
		fmt.Fprintln(out, ui.Fail(
			"could not terminate Karpenter's instances — they will keep billing, AND the node "+
				"security group cannot be deleted while they hold it, so the teardown will fail "+
				"with DependencyViolation: "+err.Error()))
		return
	}
	fmt.Fprintln(out, ui.OK("terminated "+strings.Join(insts, ", ")))
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
