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
	"encoding/json"
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
					"agent-iam will fail on DeleteConflict. Look for <cluster>-<platform>-{tenant,session} "+
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
	// Query is the read-only half. It executes even in dry-run, which is what lets a
	// dry-run of a sweep DEMONSTRATE what it would select instead of describing its own
	// filter. See exec.Runner.Query.
	Query(ctx context.Context, name string, args ...string) (string, error)
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
// Enumeration is by IAM path AND by cluster name, and BOTH halves are load-bearing.
//
// The path is /eks-agent-platform/tenants/, not /eks-agent-platform/. The operator mints
// every tenant and session role under the deeper path (its TenantIAMPath default, which the
// catalog does not override — operators/internal/controller/platform_iam.go and
// platform_session_iam.go). The shallower prefix ALSO matches the one ROLE agent-iam's own
// terraform owns and is about to destroy in the ordinary way: <cluster>-agent-platform-operator,
// at path /eks-agent-platform/ (components/aws/agent-iam/main.tf). It is named for the
// cluster, so the name filter below does not exclude it — only the path does. Sweeping it
// force-deletes a resource terraform still holds in state, so the subsequent destroy plans a
// delete for a role that no longer exists. This function exists to make a teardown succeed;
// deleting terraform's own resources out from under it is the opposite.
//
// The tenant permissions boundary and tenant baseline live at that same shallow path but are
// aws_iam_policy, not roles, so `aws iam list-roles` never returned them at either depth.
// They are the DeleteConflict victims this whole function protects, not sweep candidates.
//
// The cluster-name filter is what keeps the sweep inside the cluster being torn down. IAM is
// global and the path carries neither an environment nor a cluster segment, so an account
// hosting development and staging clusters has every cluster's tenant roles under one prefix.
// Every role under this path is named for its cluster — tenantRoleName and sessionRoleName
// both compose `clusterName + "-" + platform + suffix`, and their >64-char truncation branch
// keeps that same `clusterName + "-"` prefix. Without the filter, tearing down staging
// deletes development's live tenant roles, and the agent pods there start failing AssumeRole
// with nothing to explain why.
//
// It is a PREFIX match, not an identity check, and the difference has one reachable edge: a
// cluster whose base name extends another's with a hyphen. Tearing down `dev-plat` also
// matches `dev-plat-gpu`'s roles, because `dev-plat-` prefixes `dev-plat-gpu-web-tenant`. No
// filter over names alone can fix it — cluster `dev-hub` + platform `gpu-web` and cluster
// `dev-hub-gpu` + platform `web` compose the identical role name, and the operator's role
// tags carry PlatformId, Tenant and Environment but no cluster key. The ownership check below
// does NOT close this one: both clusters' roles are genuinely ours, so a tag that proves
// "belongs to this org" is satisfied by both. Closing it properly means intersecting these
// candidates with `kubectl get platforms` when the cluster is reachable, which costs this
// function its deliberate "needs no reachable cluster" property. Until then: do not name two
// co-located clusters where one base name is a hyphen-prefix of the other. config.Validate
// cannot catch that — it sees one cluster at a time.
//
// What the ownership check DOES close is the other half, and in a shared account it is the
// half that matters: a role at this path whose name happens to start with this cluster's name
// but which this run did not create. Nothing prevented that before — the sweep force-deleted
// on the strength of a path and a string prefix.
//
// One class of operator-minted role is NOT covered: the eventBridgeScheduler capability
// mints `<cluster>-<platform>-scheduler-invoke` at the ROOT path with no Path set
// (platform_capability_policy.go:270). The name filter below WOULD match it — upstream
// 0546a92 re-keyed it from the environment to the cluster — but the path prefix excludes it
// before the name is ever considered. It carries the tenant permissions boundary agent-iam
// destroys, so a Platform declaring that capability whose finalizer did not complete can
// still wedge a teardown on DeleteConflict.
//
// Widening the path is not the answer: enumerating IAM's root path means every role in the
// account, and this sweep must stay scoped to one cluster. The channel upstream prescribes
// is the tag sweep — 0546a92 tags these roles so a compromise sweep can pick them out of
// the root path (platform_capability_policy.go:315-322) — or accepting that the finalizer
// owns the delete, which it does whenever the operator is healthy.
// A third filter now sits after those two, and it is the one that makes the sweep provable
// rather than merely narrow: every candidate's TAGS must establish that it is ours.
//
// The path and the name are both structural guesses. The path says "something put this under
// the operator's prefix"; the name says "something named this for our cluster". Neither is a
// statement by the resource itself, and the name filter is a documented prefix match with a
// known collision (see below). In an account holding one estate that is enough, because there
// is nothing else it could be. An account that has been used for anything else breaks that,
// and the org tagging standard does not help — it pins one Project value for every deployment
// that follows it, so "it looks like ours" and "it is ours" come apart. Owner.Proves closes
// that gap; see own.go for why Repository is the only key that can. A candidate that cannot be
// proved is REPORTED and SKIPPED, never deleted.
//
// Skipping has a cost and it is named in the output: an operator-minted role that survives is
// exactly what makes agent-iam fail on DeleteConflict, so a skip predicts the teardown failure
// it declines to prevent. That is the correct trade. Force-deleting an untagged IAM role
// because its name matched is how a teardown reaches outside itself, and IAM is global — no
// region protects anything here.
func OperatorRoles(ctx context.Context, run *exec.Runner, out io.Writer, own Owner) {
	reapOperatorRoles(ctx, run, run.DryRun, out, own)
}

func reapOperatorRoles(ctx context.Context, run execer, dryRun bool, out io.Writer, own Owner) {
	const pathPrefix = "/eks-agent-platform/tenants/"

	// A blank cluster name would make the prefix filter below match everything, turning the
	// scoping this function documents into an account-wide sweep. There is no cluster whose
	// roles could be named for it, so there is nothing to reap.
	if own.Cluster == "" {
		return
	}

	if dryRun {
		fmt.Fprintln(out, ui.Step("force-delete operator-minted IAM roles under "+pathPrefix+
			" named "+own.Cluster+"-* whose tags prove they are "+own.Org+"'s (agent-iam DeleteConflict backstop)"))
	}

	// Enumeration runs in dry-run too, and the line above is why that is an addition rather
	// than a replacement. Stating the filter is worth doing — it is how an operator checks the
	// scoping is what they expect. But it is a description of intent, and a description cannot
	// be wrong in a way anyone notices. The dry-run used to stop there, so the one question a
	// dry-run of a destructive sweep exists to answer — what would this actually select? — was
	// answered by restating the filter back.
	//
	// Now it says what it will do and then does the read-only half for real, so
	// `rackctl destroy` without --apply can be pointed at a live account and SHOWN to select
	// nothing pre-existing. That is a negative test rather than a promise.
	//
	// --path-prefix is a server-side filter and the CLI auto-paginates, so this returns
	// every matching role regardless of how many Platforms existed.
	names, err := run.Query(ctx, "aws", "iam", "list-roles",
		"--path-prefix", pathPrefix,
		"--query", "Roles[].RoleName", "--output", "text")
	if err != nil {
		// Still not a teardown failure — a teardown must work against an account it cannot
		// fully reach. But it is no longer SILENT, and the difference is the point: "there are
		// no roles" and "I could not find out whether there are roles" are opposite facts, and
		// the old code rendered both as an empty line. An operator reading a clean teardown had
		// no way to tell which one they got.
		fmt.Fprintln(out, ui.Warn("could not enumerate IAM roles under "+pathPrefix+
			" ("+err.Error()+") — this backstop did NOT run. If agent-iam fails on DeleteConflict, "+
			"an operator-minted role is still there."))
		return
	}
	var candidates []string
	for _, name := range strings.Fields(strings.TrimSpace(names)) {
		if strings.HasPrefix(name, own.Cluster+"-") {
			candidates = append(candidates, name)
		}
	}
	if len(candidates) == 0 {
		if dryRun {
			fmt.Fprintln(out, ui.OK("no IAM roles under "+pathPrefix+" named "+own.Cluster+
				"-* — this sweep selects nothing"))
		}
		return
	}

	var owned, refused []string
	for _, name := range candidates {
		tags, err := roleTags(ctx, run, name)
		if err != nil {
			refused = append(refused, name+" — could not read its tags ("+err.Error()+"), so ownership is unknown")
			continue
		}
		if ok, why := own.Proves(tags); !ok {
			refused = append(refused, name+" — "+why)
			continue
		}
		owned = append(owned, name)
	}

	for _, r := range refused {
		fmt.Fprintln(out, ui.Warn("REFUSING to delete IAM role "+r))
	}
	if len(refused) > 0 {
		fmt.Fprintln(out, ui.Warn(fmt.Sprintf(
			"%d role(s) matched this cluster's name under %s but could not be proved to belong to "+
				"this run, so they were left alone. If agent-iam now fails on DeleteConflict, one of "+
				"them is why — check its tags and remove it deliberately.", len(refused), pathPrefix)))
	}
	if len(owned) == 0 {
		return
	}

	if dryRun {
		fmt.Fprintln(out, ui.Step(fmt.Sprintf(
			"(dry-run) would force-delete %d operator-minted IAM role(s): %s",
			len(owned), strings.Join(owned, ", "))))
		return
	}

	fmt.Fprintln(out, ui.Step(fmt.Sprintf(
		"force-deleting %d operator-minted IAM role(s) a finalizer left behind (agent-iam would else fail on DeleteConflict)", len(owned))))
	for _, name := range owned {
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
// The tag is a statement by the resource about which cluster owns it, which is a stronger
// proof than the Repository check own.go applies elsewhere: Repository proves "ours", this
// proves "ours AND this cluster's". So no additional ownership check is layered on here —
// requiring a second tag would only add a way for the sweep to MISS, and a missed instance
// wedges the teardown on DependencyViolation with the cluster already gone.
//
// Enumeration runs in dry-run so the selection can be shown rather than described.
func OrphanedNodes(ctx context.Context, run *exec.Runner, out io.Writer, cluster, region string) {
	if cluster == "" {
		return
	}
	ids, err := run.Query(ctx, "aws", "ec2", "describe-instances",
		"--region", region,
		"--filters",
		"Name=instance-state-name,Values=running,pending,stopping,stopped",
		"Name=tag:karpenter.sh/managed-by,Values="+cluster,
		"--query", "Reservations[].Instances[].InstanceId", "--output", "text")
	if err != nil {
		// Loud, for the same reason as the IAM sweep: an instance this misses holds the node
		// security group, and terraform cannot delete a security group that is in use. The
		// teardown then stops with the cluster gone and the instance billing. An operator who
		// was told the check did not run can look; one told nothing cannot.
		fmt.Fprintln(out, ui.Warn("could not enumerate Karpenter instances for "+cluster+
			" ("+err.Error()+") — this backstop did NOT run. A surviving node will stop the "+
			"teardown on DependencyViolation deleting the node security group."))
		return
	}
	ids = strings.TrimSpace(ids)
	if ids == "" || ids == "None" {
		if run.DryRun {
			fmt.Fprintln(out, ui.OK("no EC2 instances tagged karpenter.sh/managed-by="+cluster+
				" — this sweep selects nothing"))
		}
		return
	}

	insts := strings.Fields(ids)
	if run.DryRun {
		fmt.Fprintln(out, ui.Step(fmt.Sprintf("(dry-run) would terminate %d Karpenter instance(s): %s",
			len(insts), strings.Join(insts, ", "))))
		return
	}
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
// So this is the backstop. It deletes only volumes carrying kubernetes.io/cluster/<name>,
// which is a statement by the volume about which cluster owns it. Once the cluster is
// destroyed, no volume of its can still be attached — anything left with that tag in
// `available` state is an orphan, by definition.
//
// Runs after the cluster is destroyed, never before: an in-use volume is skipped by
// the status filter, so this cannot detach a volume from anything living.
//
// # WHAT THIS SWEEP CANNOT SEE, AND WHY IT NOW SAYS SO
//
// This function used to assert that "every dynamically provisioned volume is tagged
// kubernetes.io/cluster/<name>=owned by the EBS CSI driver". That is not established. The
// driver is an EKS MANAGED ADDON declared at landing-zone components/aws/cluster/eks.tf:92-105
// with no configuration_values block at all, so neither extraVolumeTags nor k8sTagClusterId is
// set; and the gp3 StorageClass in eks-gitops sets no tagSpecification parameters. Without
// one of those, a dynamically provisioned volume carries only the driver's provenance tags —
// CSIVolumeName and kubernetes.io/created-for/* — which say a Kubernetes cluster made it and
// do not say WHICH.
//
// The volumes that actually orphan are exactly those: Loki's 50Gi, Tempo's 50Gi, the kagent
// database. Karpenter's node volumes do get the cluster tag, but they terminate with their
// instances and were never the problem.
//
// So the filter may select nothing on a real teardown, and the old code then printed nothing
// and returned success — a sweep that reports done having looked at one tag that may not
// exist. That is the org's own recurring class (the gate is green, the function is absent)
// living inside the backstop written to catch it.
//
// The fix is not to widen the DELETE. A volume with no cluster tag cannot be proved to belong
// to this run, and an account may hold more than this deployment, so deleting on provenance
// alone is exactly the overreach the standing rule forbids. The fix is to stop being silent: enumerate
// available volumes that carry EBS CSI provenance and no cluster tag, and REPORT them, naming
// the cost. An operator who is told "three volumes are probably yours and I will not guess"
// can delete three volumes. An operator told nothing pays for them indefinitely.
//
// Tagging them properly is upstream work — a configuration_values block on the addon — and is
// filed as such. This is what rackctl can do without it.
func OrphanedVolumes(ctx context.Context, run *exec.Runner, out io.Writer, cluster, region string) {
	if cluster == "" {
		return
	}
	reportUnprovableVolumes(ctx, run, out, cluster, region)

	ids, err := run.Query(ctx, "aws", "ec2", "describe-volumes",
		"--region", region,
		"--filters",
		"Name=status,Values=available",
		"Name=tag-key,Values=kubernetes.io/cluster/"+cluster,
		"--query", "Volumes[].VolumeId", "--output", "text")
	if err != nil {
		fmt.Fprintln(out, ui.Warn("could not enumerate orphaned EBS volumes for "+cluster+
			" ("+err.Error()+") — this sweep did NOT run. Any volume the cluster left behind is "+
			"still billing; check `aws ec2 describe-volumes --filters Name=status,Values=available`."))
		return
	}
	ids = strings.TrimSpace(ids)
	if ids == "" || ids == "None" {
		if run.DryRun {
			fmt.Fprintln(out, ui.OK("no available EBS volumes tagged kubernetes.io/cluster/"+cluster+
				" — this sweep selects nothing"))
		}
		return
	}

	vols := strings.Fields(ids)
	if run.DryRun {
		fmt.Fprintln(out, ui.Step(fmt.Sprintf("(dry-run) would delete %d orphaned EBS volume(s): %s",
			len(vols), strings.Join(vols, ", "))))
		return
	}
	fmt.Fprintln(out, ui.Step(fmt.Sprintf("sweeping %d EBS volume(s) orphaned by %s", len(vols), cluster)))
	for _, v := range vols {
		if err := run.Run(ctx, "aws", "ec2", "delete-volume", "--region", region, "--volume-id", v); err != nil {
			fmt.Fprintln(out, ui.Fail("could not delete "+v+" — it will keep billing: "+err.Error()))
			continue
		}
		fmt.Fprintln(out, ui.OK("deleted "+v))
	}
}

// reportUnprovableVolumes names the available EBS volumes that a Kubernetes cluster clearly
// created and that carry nothing saying which one.
//
// It deletes nothing and is deliberately not wired to a flag that would let it. The whole
// point is the distinction the standing rule turns on: a volume tagged
// kubernetes.io/cluster/<this cluster> is provably ours and gets deleted; a volume tagged only
// CSIVolumeName is provably SOME cluster's and might be a sibling environment's in the same
// account, so it gets named and left. Deleting it would be a guess, and this account holds
// two estates besides this one.
//
// The provenance keys are the EBS CSI driver's own defaults, so a match is strong evidence of
// origin even though it is no evidence of ownership.
func reportUnprovableVolumes(ctx context.Context, run execer, out io.Writer, cluster, region string) {
	raw, err := run.Query(ctx, "aws", "ec2", "describe-volumes",
		"--region", region,
		"--filters", "Name=status,Values=available",
		"--query", "Volumes[].{VolumeId:VolumeId,Size:Size,Tags:Tags}", "--output", "json")
	if err != nil || strings.TrimSpace(raw) == "" {
		return
	}
	var vols []struct {
		VolumeId string
		Size     int
		Tags     []struct{ Key, Value string }
	}
	if err := json.Unmarshal([]byte(raw), &vols); err != nil {
		return
	}

	var unprovable []string
	for _, v := range vols {
		var csi, clusterTagged bool
		for _, t := range v.Tags {
			switch {
			case t.Key == "CSIVolumeName" || strings.HasPrefix(t.Key, "kubernetes.io/created-for/"):
				csi = true
			case strings.HasPrefix(t.Key, "kubernetes.io/cluster/"):
				clusterTagged = true
			}
		}
		if csi && !clusterTagged {
			unprovable = append(unprovable, fmt.Sprintf("%s (%dGiB)", v.VolumeId, v.Size))
		}
	}
	if len(unprovable) == 0 {
		return
	}

	fmt.Fprintln(out, ui.Warn(fmt.Sprintf(
		"%d available EBS volume(s) were provisioned by an EBS CSI driver but carry no "+
			"kubernetes.io/cluster/* tag, so rackctl cannot prove they belong to %s and will NOT delete "+
			"them: %s", len(unprovable), cluster, strings.Join(unprovable, ", "))))
	fmt.Fprintln(out, ui.Warn(
		"They will keep billing. If this teardown was the only cluster in the account they are almost "+
			"certainly its Loki/Tempo/database volumes; delete them deliberately with "+
			"`aws ec2 delete-volume --volume-id <id>`. The durable fix is upstream: set extraVolumeTags "+
			"on the aws-ebs-csi-driver addon so the volumes say which cluster made them."))
}

// PointAt aims kubectl at the cluster whose resources are about to be reaped, and reports
// whether it succeeded. A non-nil error means the ambient sweeps — All and UnstickTerminating —
// MUST be skipped.
//
// Those two run `kubectl delete platforms|tenants|nodeclaims|pvc --all -A` and patch finalizers
// off CRs against whatever context kubectl currently resolves, and their only guard is a
// /readyz probe, which tests liveness and never identity. So "which cluster am I pointed at"
// is not a detail of the sweep, it is the sweep's entire safety argument.
//
// `rackctl destroy` had no answer to that question. It never touched the kubeconfig, and the
// environment passes through, so a teardown of staging launched from a shell pointed at a
// healthy development cluster deleted every Platform, Tenant, NodeClaim and PVC in
// DEVELOPMENT and then stripped their finalizers. The engine's rollback path holds the same
// invariant through State.KubeconfigCluster; that command does not use the engine, so it has
// to establish the fact itself.
//
// Repointing rather than merely comparing is deliberate: it also fixes the aimed-at-nothing
// case. An operator whose context was stale would otherwise have the sweeps skip a cluster
// that WAS reachable under its own name, leaving controller-owned resources for the component
// teardown to trip over on a DeleteConflict or an in-use security group.
func PointAt(ctx context.Context, run *exec.Runner, cluster string) error {
	return pointAt(ctx, run, cluster)
}

func pointAt(ctx context.Context, run execer, cluster string) error {
	if cluster == "" {
		return fmt.Errorf("no cluster name to point kubectl at")
	}
	return run.Run(ctx, "aws", "eks", "update-kubeconfig", "--name", cluster)
}

// FleetSpokes returns the names of eks-fleet Cluster CRs on the hub, if any.
//
// A Cluster CR is not a Kubernetes object with a Kubernetes-shaped blast radius. The
// eks-fleet composition hands it to provider-opentofu, which applies a whole landing-zone
// module for it — a real EKS control plane, its VPC, its NAT gateways, its node roles —
// and the ClusterProviderConfig is explicitly built to vend CROSS-ACCOUNT
// (compositions/cluster-aws.yaml: "hub role same-account, fleet-vend cross-account").
//
// So the hub cluster is the only thing that knows those clusters exist. Destroy the hub
// and every spoke is orphaned: the Crossplane control plane that could have deleted them
// is gone, their terraform state sits in a bucket rackctl never touches, and no rackctl
// command will ever enumerate them again. They bill forever, in accounts this config does
// not even name.
//
// An empty result is reported for an unreachable cluster or an absent CRD, because both
// mean the same thing here: there is no hub to read spokes from.
func FleetSpokes(ctx context.Context, run *exec.Runner) []string {
	return fleetSpokes(ctx, run)
}

func fleetSpokes(ctx context.Context, run execer) []string {
	out, err := run.Capture(ctx, "kubectl", "get", "clusters.fleet.nanohype.dev",
		"-A", "-o", `jsonpath={range .items[*]}{.metadata.namespace}{"/"}{.metadata.name}{" "}{end}`)
	if err != nil {
		return nil // no CRD installed, or no reachable cluster — either way, no spokes to strand
	}
	return strings.Fields(out)
}
