// Package phases implements the ordered 0→running bootstrap. Each phase
// orchestrates the existing nanohype repos — landing-zone (Terragrunt),
// eks-gitops (ArgoCD catalog), eks-agent-platform (operator). rackctl is the
// glue that automates landing-zone/docs/first-deploy-aws.md, NOT a rewrite.
package phases

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/engine"
	"github.com/rackctl/rackctl/internal/exec"
	"github.com/rackctl/rackctl/internal/gitops"
	"github.com/rackctl/rackctl/internal/reap"
	"github.com/rackctl/rackctl/internal/tf"
)

// CoreComponents returns the landing-zone apply order for the core path; destroy
// runs it in reverse.
//
// The list is derived from the config rather than fixed, because three components
// are conditional and the old fixed list omitted all three — so a config that asked
// for them applied nothing, and the cluster came up subtly broken:
//
//   - agent-iam creates the eks-agent-platform operator's IAM role. Without it the
//     operator crashloops on AssumeRoleWithWebIdentity 403 — it is not optional
//     whenever the agent platform is installed, which is the default.
//
//   - managed-monitoring provisions AMP + AMG and writes the endpoints to SSM.
//     cluster-bootstrap READS those SSM params (grafana_url, amp_endpoint,
//     amp_workspace_id) to stamp them onto the ArgoCD cluster Secret, so it must be
//     applied BEFORE cluster-bootstrap or the read fails. It is gated on
//     addons.observability because AMP and AMG both cost money — it is never
//     applied unless asked for.
//
//   - dns creates the hosted zone + external-dns identity; gated on a dns block.
//
// Ordering is load-bearing and is NOT enforced by terragrunt (these roots declare
// no dependency blocks) — this slice is the only thing that sequences them.
func CoreComponents(cfg *config.Config) []string {
	comps := []string{"network", "cluster", "secrets"}
	if cfg.AgentPlatform.Enabled() {
		comps = append(comps, "agent-iam")
	}
	if cfg.Addons.Observability {
		comps = append(comps, "managed-monitoring") // must precede cluster-bootstrap
	}
	if cfg.DNS != nil && cfg.DNS.HostedZone != "" {
		comps = append(comps, "dns")
	}

	// cluster-addons before cluster-bootstrap. This documents the order; the two are
	// APPLIED by different phases (substrate and gitops respectively), and it is that phase
	// boundary — not this slice — that actually enforces "the AWS substrate exists before
	// ArgoCD consumes it". See the substrate phase for why that ordering is load-bearing
	// (Pod Identity is injected at pod admission) and why it must be structural.
	return append(comps, "cluster-addons", "cluster-bootstrap")
}

// All returns the ordered bootstrap pipeline. Phases 0–6 are the core
// 0→running path (AWS-only, v1); 7–9 are opt-in layers.
func All() []engine.Phase {
	return []engine.Phase{
		preflight{base{id: "preflight", title: "Preflight — tools, identity, quotas"}},
		acquire{base{id: "acquire", title: "Acquire platform repos (clone + fork)"}},
		identity{base{id: "identity", title: "Identity & Terraform state backend"}},
		cluster{base{id: "cluster", title: "Network & EKS cluster"}},
		substrate{base{id: "substrate", title: "AWS substrate — IAM, Pod Identity, buckets, monitoring"}},
		gitopsPhase{base{id: "gitops", title: "ArgoCD GitOps & addon convergence"}},
		platform{base{id: "platform", title: "Agent-platform substrate, CRDs & operator"}},
		fleet{base{id: "fleet", title: "Cluster control plane (eks-fleet)", optional: true,
			enabled: func(st *engine.State) bool { return st.Config.ControlPlane.EKSFleet }}},
		portal{base{id: "portal", title: "Operator portal (day-2 UI)", optional: true,
			enabled: func(st *engine.State) bool { return st.Config.ControlPlane.Portal }}},
		smoke{base{id: "smoke", title: "First-tenant smoke test", optional: true,
			enabled: func(st *engine.State) bool { return st.Config.FirstTenant != nil }}},
	}
}

type base struct {
	id, title string
	optional  bool
	enabled   func(*engine.State) bool
}

func (b base) ID() string     { return b.id }
func (b base) Title() string  { return b.title }
func (b base) Optional() bool { return b.optional }
func (b base) Enabled(st *engine.State) bool {
	if b.enabled == nil {
		return true
	}
	return b.enabled(st)
}

// Teardown is a no-op by default; phases that create billable cloud resources
// override it (cluster, substrate, gitops, platform) so the engine's rollback
// actually destroys them.
func (base) Teardown(context.Context, *engine.State) error { return nil }

func note(st *engine.State, format string, a ...any) {
	fmt.Fprintf(st.Runner.Out, "    "+format+"\n", a...)
}

// componentDir is the landing-zone Terragrunt path for a component. The live
// layout is live/aws/<account>/<region>/<env>/<component>, where the account
// dir is workload-<env> (e.g. live/aws/workload-development/us-west-2/development/network).
func componentDir(st *engine.State, component string) string {
	env := string(st.Config.Environment)
	return fmt.Sprintf("live/aws/workload-%s/%s/%s/%s", env, st.Config.Cloud.Region, env, component)
}

// apply / destroy run a landing-zone Terragrunt component for the current env.
func apply(ctx context.Context, st *engine.State, component string) error {
	return tg(ctx, st, "apply", component)
}
func destroy(ctx context.Context, st *engine.State, component string) error {
	return tg(ctx, st, "destroy", component)
}
func tg(ctx context.Context, st *engine.State, verb, component string) error {
	dir := componentDir(st, component)

	// Always init first.
	//
	// Terragrunt's auto-init only fires when `.terraform` is ABSENT. It does not fire
	// when the source gained a module — and .terragrunt-cache lives in the checkout and
	// survives every run. So a component that acquires a new `module` block is copied
	// into a cache whose .terraform/modules/modules.json was written before that module
	// existed, and tofu dies at apply time:
	//
	//	│ Error: Module not installed
	//	│   on main.tf line 326:
	//	│  326: module "grafana_token_rotator_irsa" {
	//
	// That took down a run that had already built a VPC, an EKS cluster and two nodes —
	// the rollback then destroyed 40 resources to unwind it. The cache was from the
	// previous day and knew about exactly one module; the component now had two.
	//
	// This is the third face of the same bug (see cloneOrUpdate and forkOrSync): a reused
	// artifact treated as current because it is present. Terraform's own contract is that
	// init follows any change to modules, and rackctl is the thing that just changed them
	// — by pulling landing-zone. So it inits, rather than betting on a heuristic.
	//
	// It is cheap: providers are already in the cache, so init verifies rather than
	// downloads. And it runs before `destroy` too — a teardown needs its modules
	// installed exactly as much as an apply does, which is why the rollback's own
	// `teardown gitops` failed with the same error.
	if err := st.Runner.Run(ctx, "terragrunt", "--working-dir", dir, "--non-interactive", "init"); err != nil {
		return err
	}

	// terragrunt 1.0+ takes global flags (--working-dir, --non-interactive) before
	// the command; -auto-approve is a tofu flag after it. The old post-command
	// --terragrunt-working-dir is silently ignored by 1.0.x (runs in the cwd).
	return st.Runner.Run(ctx, "terragrunt", "--working-dir", dir, "--non-interactive", verb, "-auto-approve")
}

// captureOutputs merges a component's `terragrunt output -json` into State. It
// is a no-op in dry-run and on any error (outputs are advisory, not required).
func captureOutputs(ctx context.Context, st *engine.State, component string) {
	if st.Runner.DryRun {
		return
	}
	dir := componentDir(st, component)
	data, err := st.Runner.Capture(ctx, "terragrunt", "--working-dir", dir, "output", "-json")
	if err != nil || data == "" {
		return
	}
	m, err := tf.ParseOutputs([]byte(data))
	if err != nil {
		return
	}
	for k, v := range m {
		st.Outputs[k] = v
	}
	note(st, "captured %d terragrunt output(s) from %s", len(m), component)
}

// --- Phase 0: preflight ---
type preflight struct{ base }

func (preflight) Run(ctx context.Context, st *engine.State) error {
	if err := exec.RequireTools("tofu", "terragrunt", "kubectl", "helm", "aws", "git", "gh"); err != nil {
		return err
	}
	// Verify the caller is authenticated and points at the configured account —
	// failing here beats a confusing failure three phases into provisioning.
	account, err := st.Runner.Capture(ctx, "aws", "sts", "get-caller-identity", "--query", "Account", "--output", "text")
	if err != nil {
		return fmt.Errorf("aws auth check failed (run `aws sso login`): %w", err)
	}
	if account != "" && account != st.Config.Cloud.AccountID {
		return fmt.Errorf("caller account %s does not match cloud.accountId %s", account, st.Config.Cloud.AccountID)
	}
	// EC2 vCPU quota (L-1216C47A): fresh accounts cap ~32, which strands the
	// cluster apply mid-provision. Read it, and file an increase if requested.
	note(st, "checking EC2 vCPU quota L-1216C47A (target %d)", st.Config.Quotas.VCPU)
	if err := st.Runner.Run(ctx, "aws", "service-quotas", "get-service-quota",
		"--service-code", "ec2", "--quota-code", "L-1216C47A"); err != nil {
		return err
	}
	if st.Config.Quotas.AutoRequest {
		// Only file an increase if we are actually below the target. Service Quotas
		// rejects a request for a value at or below the current one:
		//
		//   IllegalArgumentException: You must provide a quota value greater than the
		//   current quota value
		//
		// which is not a failure — it means the quota is already sufficient. Asking
		// anyway logged an ERROR on every run of an account that was already fine.
		if cur, err := currentVCPUQuota(ctx, st); err == nil && cur >= float64(st.Config.Quotas.VCPU) {
			note(st, "vCPU quota already %.0f (>= %d) — no increase needed", cur, st.Config.Quotas.VCPU)
			return nil
		}
		note(st, "requesting vCPU quota increase to %d", st.Config.Quotas.VCPU)
		// Ignore the error: a duplicate/pending request is expected and benign.
		_ = st.Runner.Run(ctx, "aws", "service-quotas", "request-service-quota-increase",
			"--service-code", "ec2", "--quota-code", "L-1216C47A",
			"--desired-value", fmt.Sprintf("%d", st.Config.Quotas.VCPU))
	}
	return nil
}

// currentVCPUQuota reads the account's applied EC2 vCPU quota (L-1216C47A).
func currentVCPUQuota(ctx context.Context, st *engine.State) (float64, error) {
	out, err := st.Runner.Capture(ctx, "aws", "service-quotas", "get-service-quota",
		"--service-code", "ec2", "--quota-code", "L-1216C47A",
		"--query", "Quota.Value", "--output", "text")
	if err != nil {
		return 0, err
	}
	return strconv.ParseFloat(strings.TrimSpace(out), 64)
}

// --- Phase 1: acquire repos ---
type acquire struct{ base }

// cloneOrUpdate clones url into dir, or brings an existing checkout up to date.
//
// Two things this must get right, and the naive version gets neither.
//
// `git clone` fails outright if the target exists, so a rerun of init used to die
// before doing anything. Reruns are the NORMAL case: the engine's rollback destroys
// cloud resources but deliberately does not delete the operator's repos or working
// copies, so the second invocation always finds them.
//
// But merely REUSING what is there is worse than failing. These checkouts are the
// infrastructure code — landing-zone is what terragrunt applies. A stale clone means a
// rerun silently provisions with the code from the last run, so a fix you just merged
// is not in the cluster you just built, and the run that was supposed to prove it
// disproves it instead. "Present" is not "current".
//
// So: pull. --ff-only, because rackctl owns this directory but the operator may have
// touched it, and a divergence must be reported rather than merged over. It is not
// fatal — a dirty working copy is the operator's business, and the note says which
// checkout it is — but they are told.
func cloneOrUpdate(ctx context.Context, st *engine.State, url, dir string) error {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return st.Runner.Run(ctx, "git", "clone", url, dir)
	}
	prev := st.Runner.Dir
	st.Runner.Dir = dir
	defer func() { st.Runner.Dir = prev }()

	if err := st.Runner.Run(ctx, "git", "pull", "--ff-only"); err != nil {
		note(st, "%s: could not fast-forward — it has diverged from upstream, and this run "+
			"will use the code as it stands on disk", filepath.Base(dir))
		return nil
	}
	note(st, "%s updated to latest", filepath.Base(dir))
	return nil
}

// forkOrSync forks the upstream catalog into org, or — if the fork is already there —
// brings it up to date with upstream.
//
// `gh repo fork` returns HTTP 403 "Name already exists on this account" when the fork
// is there, which is not an error — it is the desired state. Treating it as one meant
// that once a fork existed, init could NEVER run again:
//
//	failed to fork: HTTP 403: Name already exists on this account
//	✗ [2/10] Acquire platform repos — gh: exit status 1
//
// And a fork always exists after the first attempt, because the rollback (rightly)
// does not delete the operator's GitHub repo. So every retry after any failure died
// here, before touching the cloud.
//
// But "the fork exists" is not "the fork is current" — the same distinction
// cloneOrUpdate exists to make, and reusing it unsynced is the same bug wearing a
// different hat. The catalog is the source of truth for everything ArgoCD runs, and
// the cluster reads it from the FORK, never from upstream. So a fork left at whatever
// commit it was forked at means a fix merged upstream this morning is simply not in
// the cluster built this afternoon — and the run meant to prove that fix quietly
// disproves it. Nothing errors. The catalog is valid; it is just old.
//
// So: sync. Fast-forward only — `gh repo sync` hard-resets ONLY with --force, which is
// deliberately not passed. The org owns this fork and is expected to commit to it; that
// is the entire point of forking the catalog rather than consuming it. A divergence is
// therefore legitimate and must never be overwritten. It is reported, and the run
// continues against the fork as it stands.
func forkOrSync(ctx context.Context, st *engine.State, org string) error {
	fork := org + "/eks-gitops"

	if _, err := st.Runner.Capture(ctx, "gh", "repo", "view", fork, "--json", "name"); err != nil || st.Runner.DryRun {
		note(st, "forking nanohype/eks-gitops → %s (the operator owns the addon catalog for IRSA writeback)", fork)
		return st.Runner.Run(ctx, "gh", "repo", "fork", "nanohype/eks-gitops",
			"--org", org, "--fork-name", "eks-gitops", "--clone=false")
	}

	note(st, "%s already exists — syncing it with upstream", fork)
	if err := st.Runner.Run(ctx, "gh", "repo", "sync", fork,
		"--source", "nanohype/eks-gitops", "--branch", "main"); err != nil {
		note(st, "%s: could not fast-forward — it has diverged from nanohype/eks-gitops, and this "+
			"run will use the catalog as it stands on the fork. If that is not intended, reconcile "+
			"the fork before re-running.", fork)
	}
	return nil
}

func (acquire) Run(ctx context.Context, st *engine.State) error {
	org := st.Config.Org.Name
	st.Repos = engine.RepoPaths(org)
	note(st, "cloning platform repos into %s", st.Repos.Workdir)
	if err := cloneOrUpdate(ctx, st, "https://github.com/nanohype/landing-zone.git", st.Repos.LandingZone); err != nil {
		return err
	}
	if err := cloneOrUpdate(ctx, st, "https://github.com/nanohype/eks-agent-platform.git", st.Repos.AgentPlatform); err != nil {
		return err
	}
	if err := forkOrSync(ctx, st, org); err != nil {
		return err
	}
	// Clone the fork to the exact path (gh's --clone ignores the target dir).
	if err := cloneOrUpdate(ctx, st,
		fmt.Sprintf("https://github.com/%s/eks-gitops.git", org), st.Repos.EKSGitops); err != nil {
		return err
	}
	// The portal chart is not published to ghcr; clone its repo so the portal
	// phase can install from the local chart (mirrors the operator fallback).
	if st.Config.ControlPlane.Portal {
		note(st, "cloning nanohype/portal (day-2 UI) for its local chart")
		return cloneOrUpdate(ctx, st, "https://github.com/nanohype/portal.git", st.Repos.Portal)
	}
	return nil
}

// --- Phase 2: identity & state backend ---
type identity struct{ base }

func (identity) Run(ctx context.Context, st *engine.State) error {
	st.Runner.Dir = st.Repos.LandingZone
	c := st.Config
	note(st, "generating account.hcl (account %s) and the versioned S3 tfstate backend", c.Cloud.AccountID)
	return st.Runner.Run(ctx, "scripts/init-backend-aws.sh", c.Cloud.AccountID, c.Cloud.Region)
}

// --- Phase 3: network & cluster ---
type cluster struct{ base }

func (cluster) Run(ctx context.Context, st *engine.State) error {
	st.Runner.Dir = st.Repos.LandingZone
	// The cluster base rides TF_VAR_cluster_name into landing-zone's network + cluster
	// modules (their var.cluster_name), which compose <environment>-<cluster_name> — the
	// same string ClusterName() returns. network.hcl no longer pins cluster_name in its
	// inputs, so this TF_VAR is what names the cluster and its VPC subnet-discovery tags;
	// network and cluster must agree on it or Karpenter/ELB discovery breaks.
	st.Runner.Env = append(st.Runner.Env, "TF_VAR_cluster_name="+st.Config.Cluster.Name)

	// The endpoint posture rides the same seam. landing-zone's committed cluster tree is
	// private-by-default and fail-closed — a public API endpoint with no allow-list is
	// rejected at plan time. rackctl owns the fragile per-run input: it supplies the bool
	// and, when public, the CIDR allow-list (auto-detecting the operator's egress IP when
	// none is given). Both are cluster-component variables, so they belong here, layered
	// over the generic committed tree exactly like TF_VAR_cluster_name.
	endpointEnv, err := clusterEndpointEnv(ctx, st)
	if err != nil {
		return err
	}
	st.Runner.Env = append(st.Runner.Env, endpointEnv...)

	note(st, "provisioning VPC then EKS control plane (network → cluster; strict ordering)")
	for _, comp := range []string{"network", "cluster"} {
		if err := apply(ctx, st, comp); err != nil {
			return err
		}
	}
	captureOutputs(ctx, st, "cluster")
	return st.Runner.Run(ctx, "aws", "eks", "update-kubeconfig", "--name", st.Config.ClusterName())
}

func (cluster) Teardown(ctx context.Context, st *engine.State) error {
	st.Runner.Dir = st.Repos.LandingZone
	for _, comp := range []string{"cluster", "network"} { // reverse of apply
		if err := destroy(ctx, st, comp); err != nil {
			return err
		}
	}
	// The cluster is gone, so nothing of its can still be attached. Anything still
	// tagged for it is an orphan by definition — sweep it, or it bills forever.
	reap.OrphanedVolumes(ctx, st.Runner, os.Stdout,
		st.Config.ClusterName(), st.Config.Cloud.Region)
	return nil
}

// --- Phase 4: secrets & ArgoCD bootstrap ---
type bootstrap struct{ base }

// substrateComponents is the AWS substrate the GitOps layer consumes: every landing-zone
// component ArgoCD depends on but that does not itself need ArgoCD. Derived from
// CoreComponents (never restated) so the conditional components (agent-iam,
// managed-monitoring, dns) can only ever be applied in the one order CoreComponents
// documents — restating the list is what let those three silently go missing once.
//
// It is CoreComponents minus the components other phases own: network and cluster (the
// cluster phase), and cluster-bootstrap (the gitops phase — ArgoCD is the CONSUMER of the
// substrate, not part of it).
func substrateComponents(cfg *config.Config) []string {
	all := CoreComponents(cfg)
	out := make([]string, 0, len(all))
	for _, c := range all {
		switch c {
		case "network", "cluster": // the cluster phase
			continue
		case "cluster-bootstrap": // the gitops phase
			continue
		}
		out = append(out, c)
	}
	return out
}

// --- Phase 4: platform substrate (the AWS layer the catalog consumes) ---
//
// Everything ArgoCD will read must exist before ArgoCD does. This phase builds all of it —
// IAM, Pod Identity associations, S3 buckets, KMS, the AMP/AMG workspaces and their SSM
// parameters — and then writes the account id into the fork, so the catalog ArgoCD clones
// in the next phase is already correct.
//
// The phase boundary IS the dependency, and that is the point of drawing it here.
// cluster-addons creates the eleven Pod Identity associations, and EKS injects Pod Identity
// at pod ADMISSION — so a pod that starts before its association exists silently falls back
// to the node role and fails later as a permission error naming the node, not the pod. When
// cluster-addons was applied in the same breath as ArgoCD (or, worse, after it),
// external-secrets came up on the node role and the failure cascaded through alloy →
// opencost → dashboards.
//
// Substrate first, consumer second, as SEPARATE phases, is what makes that ordering
// impossible to regress. The bug survived one round of "fixing" precisely because the
// ordering lived in a component's index within a shared list rather than in the structure;
// a phase boundary cannot be quietly reordered.
type substrate struct{ base }

func (substrate) Run(ctx context.Context, st *engine.State) error {
	st.Runner.Dir = st.Repos.LandingZone
	note(st, "building the AWS substrate the catalog consumes (IAM, Pod Identity, buckets, monitoring)")
	for _, comp := range substrateComponents(st.Config) {
		if err := apply(ctx, st, comp); err != nil {
			return err
		}
	}
	captureOutputs(ctx, st, "cluster-addons")

	// IRSA writeback belongs HERE — after the substrate is built, before ArgoCD exists.
	//
	// It stamps this account's id into the fork's values and pushes, so the catalog ArgoCD
	// clones in the next phase already resolves to this account's buckets. It is the handoff
	// from "the AWS substrate is ready" to "the catalog can be pointed at it", which is
	// exactly this phase's job. Running it AFTER ArgoCD (as the old addons phase did) meant
	// ArgoCD first synced placeholder values and then had to self-heal after the push; done
	// here, there is nothing to correct.
	env := string(st.Config.Environment)
	if st.Runner.DryRun {
		note(st, "FOOTGUN GUARD: (apply) substitutes the account id into eks-gitops/addons/*/values-%s.yaml, then commits & pushes the fork", env)
		return nil
	}
	note(st, "IRSA writeback: substituting account id into eks-gitops/addons/*/values-%s.yaml", env)
	n, changed, err := gitops.WriteBack(st.Repos.EKSGitops, env, st.Config.Cloud.AccountID)
	if err != nil {
		return err
	}
	note(st, "replaced %d placeholder(s) across %d file(s)", n, len(changed))
	if len(changed) > 0 {
		st.Runner.Dir = st.Repos.EKSGitops
		// Stage by name (never `git add -A`).
		if err := st.Runner.Run(ctx, "git", append([]string{"add"}, changed...)...); err != nil {
			return err
		}
		if err := st.Runner.Run(ctx, "git", "commit", "-m", "rackctl: substitute IRSA account id ("+env+")"); err != nil {
			return err
		}
		if err := st.Runner.Run(ctx, "git", "push"); err != nil {
			return err
		}
		st.Runner.Dir = st.Repos.LandingZone
	}
	return nil
}

func (substrate) Teardown(ctx context.Context, st *engine.State) error {
	st.Runner.Dir = st.Repos.LandingZone
	comps := substrateComponents(st.Config)
	for i := len(comps) - 1; i >= 0; i-- { // reverse of apply
		if err := destroy(ctx, st, comps[i]); err != nil {
			return err
		}
	}
	return nil
}

// --- Phase 5: ArgoCD GitOps & addon convergence ---
//
// Installs ArgoCD + app-of-apps, pointed at the fork the substrate phase already made
// correct, and waits for the catalog to converge. It creates no AWS resources the addons
// depend on — the substrate phase built all of those first, which is the whole reason this
// is a separate, later phase.
type gitopsPhase struct{ base }

func (gitopsPhase) Run(ctx context.Context, st *engine.State) error {
	st.Runner.Dir = st.Repos.LandingZone
	note(st, "installing ArgoCD + app-of-apps pointing at %s", st.Config.Org.GitOps.GitURL())
	if err := apply(ctx, st, "cluster-bootstrap"); err != nil {
		return err
	}

	note(st, "waiting for ArgoCD applications to converge (sync-waves 0→52)")
	if err := st.Runner.Run(ctx, "kubectl", "-n", "argocd", "wait", "--for=condition=Healthy",
		"applications", "--all", "--timeout=30m"); err != nil {
		// The cloud is provisioned. ArgoCD is running and has generated the catalog.
		// Something on the cluster has not settled — which is NOT a reason to destroy
		// the cluster.
		//
		// `kubectl wait` fails with a bare "exit status 1" and names nothing, so say
		// what is actually unhealthy. Some apps legitimately converge slowly: opencost
		// crashloops until metrics reach AMP, which cannot happen until alloy has been
		// scraping for a few minutes. A 30-minute wait that expires with 42 of 44
		// Applications Healthy is not a failed install — and rolling the cluster back
		// destroys the only surface the remaining two can be diagnosed on.
		unhealthy, _ := st.Runner.Capture(ctx, "kubectl", "-n", "argocd", "get", "applications",
			"-o", `jsonpath={range .items[?(@.status.health.status!='Healthy')]}{.metadata.name}{" ("}{.status.health.status}{"/"}{.status.sync.status}{") "}{end}`)
		if s := strings.TrimSpace(unhealthy); s != "" {
			note(st, "not converged: %s", s)
		}
		return &engine.NoRollbackError{Err: fmt.Errorf(
			"ArgoCD applications did not all reach Healthy within 30m. The cloud IS provisioned and " +
				"the cluster is left standing — run `rackctl doctor` to see what has not settled, and " +
				"`rackctl destroy` if you want it gone")}
	}
	return nil
}

func (gitopsPhase) Teardown(ctx context.Context, st *engine.State) error {
	// Just cluster-bootstrap (ArgoCD). The engine tears phases down in reverse, so this runs
	// BEFORE substrate.Teardown — ArgoCD is stopped before the Pod Identity associations and
	// buckets it depends on are removed, which is the order you want.
	st.Runner.Dir = st.Repos.LandingZone
	return destroy(ctx, st, "cluster-bootstrap")
}

// --- Phase 6: agent-platform substrate, CRDs & operator ---
type platform struct{ base }

// agentPlatformCRDs are CRDs from each of the operator's three API groups. If these
// are established, the operator's chart has been applied.
var agentPlatformCRDs = []string{
	"platforms.platform.nanohype.dev",
	"agentfleets.agents.nanohype.dev",
	"budgetpolicies.governance.nanohype.dev",
}

// Run WAITS for the agent operator; it does not install it.
//
// The GitOps catalog owns the operator. The addons-agent-operator ApplicationSet
// deploys charts/operator from the eks-agent-platform repo (a multi-source
// Application: the chart from the product repo, its values from this org's catalog
// fork), gated on the eks-agent-platform/enabled label that cluster-bootstrap stamps
// on the ArgoCD cluster Secret. The chart carries its own crds/, so the CRDs come
// with it.
//
// This phase used to `helm upgrade --install operator` on top of that — a SECOND,
// competing Helm release of the same chart, racing ArgoCD for ownership of the same
// Deployment, ClusterRoles and CRDs. It pulled oci://ghcr.io/nanohype/charts/operator,
// which does not exist (the release workflow's chart-push-to-OCI step is skipped, and
// that path 403s), then silently fell back to a local clone — so the cluster ran an
// operator installed from a working copy on the machine that happened to run rackctl,
// while ArgoCD believed it owned one from git.
//
// GitOps owns what runs on the cluster; rackctl orchestrates the substrate underneath
// it and then verifies. So: wait for the CRDs to be established and the operator to be
// Available, and fail loudly if the catalog did not deliver them — which is a real
// failure (a missing enable label, an appset that never generated) and must not be
// papered over by installing it a second way.
func (platform) Run(ctx context.Context, st *engine.State) error {
	if !st.Config.AgentPlatform.Enabled() {
		note(st, "agentPlatform.enable=false — skipping the agent operator")
		return nil
	}
	note(st, "agent operator + CRDs are owned by the GitOps catalog (addons-agent-operator); waiting for convergence")
	if arn := st.Outputs["operator_role_arn"]; arn != "" {
		note(st, "operator role: %s", arn)
	}

	for _, crd := range agentPlatformCRDs {
		if err := st.Runner.Run(ctx, "kubectl", "wait", "--for=condition=Established",
			"crd/"+crd, "--timeout=10m"); err != nil {
			return fmt.Errorf("agent-platform CRD %s never established — the catalog did not deliver the operator chart "+
				"(check the eks-agent-platform/enabled label on the ArgoCD cluster Secret, and the "+
				"addons-agent-operator ApplicationSet): %w", crd, err)
		}
	}

	if err := st.Runner.Run(ctx, "kubectl", "-n", "eks-agent-platform", "wait",
		"--for=condition=Available", "deploy/eks-agent-platform-operator", "--timeout=10m"); err != nil {
		return fmt.Errorf("agent operator never became Available: %w", err)
	}
	return nil
}

// Teardown is a no-op: the operator is an ArgoCD Application, so it is removed when
// the cluster is. Uninstalling a Helm release rackctl no longer creates would fail,
// and deleting it out from under ArgoCD would just make ArgoCD put it back.
func (platform) Teardown(context.Context, *engine.State) error { return nil }

// --- Phase 7 (optional): eks-fleet cluster control plane ---
type fleet struct{ base }

func (fleet) Run(ctx context.Context, st *engine.State) error {
	note(st, "installing Crossplane v2 hub + eks-fleet compositions; future clusters become Cluster CRs in %s",
		st.Config.Org.GitOps.ClustersRepo)
	return st.Runner.Run(ctx, "kubectl", "apply", "-f", "eks-fleet/crossplane.yaml")
}

// --- Phase 8 (optional): operator portal ---
type portal struct{ base }

func (portal) Run(ctx context.Context, st *engine.State) error {
	note(st, "deploying portal (Go API + River worker + React); needs Postgres/Redis/S3")
	note(st, "wiring GitOps deploy keys for %s and %s", st.Config.Org.GitOps.ClustersRepo, st.Config.Org.GitOps.TenantsRepo)
	// The portal OCI chart is not published yet; fall back to the local chart in
	// the cloned repo when the pull fails (mirrors the operator).
	if err := st.Runner.Run(ctx, "helm", "upgrade", "--install", "portal",
		"oci://ghcr.io/nanohype/charts/portal"); err != nil {
		note(st, "portal OCI chart unavailable — falling back to local ./deploy/helm/portal")
		st.Runner.Dir = st.Repos.Portal
		return st.Runner.Run(ctx, "helm", "upgrade", "--install", "portal", "deploy/helm/portal")
	}
	return nil
}

func (portal) Teardown(ctx context.Context, st *engine.State) error {
	return st.Runner.Run(ctx, "helm", "uninstall", "portal", "--ignore-not-found")
}

// --- Phase 9 (optional): first-tenant smoke test ---
type smoke struct{ base }

func (smoke) Run(ctx context.Context, st *engine.State) error {
	ft := st.Config.FirstTenant
	st.Runner.Dir = st.Repos.AgentPlatform
	note(st, "installing first tenant %q (persona=%s) from charts/tenant, then waiting for Ready", ft.Name, ft.Persona)
	if err := st.Runner.Run(ctx, "helm", "upgrade", "--install", ft.Name, "charts/tenant",
		"--set", "tenant="+ft.Tenant,
		"--set", "persona="+ft.Persona,
		"--set", fmt.Sprintf("budget.monthlyUsd=%d", ft.MonthlyBudgetUSD)); err != nil {
		return err
	}
	return st.Runner.Run(ctx, "kubectl", "wait", "--for=condition=Ready",
		"platform/"+ft.Name, "--timeout=15m")
}
