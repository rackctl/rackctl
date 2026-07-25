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
// The list is derived from the config rather than fixed, because four components
// are conditional and the old fixed list omitted three of them — so a config that asked
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
//   - model-import provisions the account+region substrate for Bedrock Custom Model
//     Import — an S3 staging bucket and the IAM role Bedrock assumes during a
//     CreateModelImportJob. Unlike managed-monitoring, its POSITION here is not
//     load-bearing: its live leaf declares no terragrunt dependency, the component reads
//     only aws_caller_identity and aws_partition, and nothing on the platform resolves
//     its SSM parameters programmatically (only the human import runbook does). It sits
//     last among the conditionals for readability, and a future reader may move it. It is
//     gated because it is per-environment substrate an org may never want, and it is
//     destroyed with everything else — landing-zone scopes it by environment and gives it a
//     teardown posture (force_destroy unconditional in development, opt-in elsewhere), so
//     there is nothing here that has to outlive the cluster that asked for it.
//
// Ordering is load-bearing and terragrunt's own dependency graph does not express it:
// the orderings that matter here are substrate-before-consumer (cluster-addons' Pod
// Identity associations before ArgoCD deploys the pods that need them; managed-monitoring's
// SSM parameters before cluster-bootstrap reads them), and no `dependency` block in these
// roots encodes that. This slice, plus the phase boundary that splits it, is what
// sequences them.
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
	if cfg.AgentPlatform.Enabled() && cfg.AgentPlatform.ModelImport {
		comps = append(comps, "model-import")
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

// apply / destroy run a landing-zone Terragrunt component for the current env, with the
// TF_VARs that component declares and no others.
//
// Scoping is the point. The Runner is shared by every phase for the whole run, so the old
// idiom — `st.Runner.Env = append(st.Runner.Env, ...)` in the cluster phase — did not
// configure the cluster component, it configured EVERY terragrunt invocation that followed
// it. TF_VAR_cluster_name reached secrets, agent-iam, managed-monitoring, dns, cluster-addons
// and cluster-bootstrap, whose own envcommon simultaneously hands them
// `cluster_name = <environment>-<base>`. And an ambient TF_VAR beats a terragrunt `inputs`
// value, so the leaked base name won that argument silently.
//
// A variable is scoped to the invocation that needs it, or it is a global — there is no
// third thing, and `cmd/tgenv.go` is where the genuine globals live.
func apply(ctx context.Context, st *engine.State, component string) error {
	env, err := componentEnv(ctx, st, component)
	if err != nil {
		return err
	}
	return tg(ctx, st, "apply", component, env...)
}

func destroy(ctx context.Context, st *engine.State, component string) error {
	env, err := componentEnv(ctx, st, component)
	if err != nil {
		return err
	}
	return tg(ctx, st, "destroy", component, env...)
}

// Destroy runs one component's teardown with its scoped env. Exported for `rackctl destroy`,
// which walks CoreComponents in reverse outside the phase engine.
//
// It exists so that path cannot drift from this one. It used to restate the init+destroy
// sequence itself and build its env from tgEnv alone — so a standalone `rackctl destroy`
// passed none of the per-component variables the apply had, and the cluster component fell
// back to its own default name. Restating what a shared helper already does is the mistake
// substrateComponents was written to prevent; this is the same mistake one layer down.
func Destroy(ctx context.Context, st *engine.State, component string) error {
	return destroy(ctx, st, component)
}

// componentEnv returns the TF_VARs a single landing-zone component declares. Components not
// named here take nothing beyond the globals in tgEnv.
//
// Every entry must correspond to a variable that component actually declares. TF_VAR_cluster_name
// used to be injected into `network` as well, on the stated grounds that "network and cluster
// must agree on it or Karpenter/ELB discovery breaks" — but components/aws/network declares no
// cluster_name variable at all, and its own comment says the cluster-ownership and
// Karpenter-discovery tags are per-cluster and applied by the CLUSTER component via
// aws_ec2_tag, precisely because the VPC is shared per environment and cluster-agnostic.
// The injection was inert and the reasoning was backwards.
func componentEnv(ctx context.Context, st *engine.State, component string) ([]string, error) {
	switch component {
	case "network":
		// The create-mode levers are network variables: ipam_pool_id, ipam_netmask_length,
		// transit_gateway_id, centralized_egress.
		return clusterNetworkEnv(st), nil
	case "cluster":
		// cluster_name plus the endpoint posture, all three cluster variables. The endpoint
		// builder may detect this host's egress IP, so it can fail and must be able to say so.
		env := []string{"TF_VAR_cluster_name=" + st.Config.Cluster.Name}
		endpointEnv, err := clusterEndpointEnv(ctx, st)
		if err != nil {
			return nil, err
		}
		return append(env, endpointEnv...), nil
	default:
		return nil, nil
	}
}

func tg(ctx context.Context, st *engine.State, verb, component string, extraEnv ...string) error {
	dir := componentDir(st, component)

	// Scope extraEnv to this invocation — both commands below, then restored. Copied rather
	// than appended in place so the restore cannot be defeated by a shared backing array.
	if len(extraEnv) > 0 {
		prev := st.Runner.Env
		st.Runner.Env = append(append([]string(nil), prev...), extraEnv...)
		defer func() { st.Runner.Env = prev }()
	}

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
		note(st, "forking nanohype/eks-gitops → %s (ArgoCD syncs the catalog from the org's fork, never upstream)", fork)
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
		if err := cloneOrUpdate(ctx, st, "https://github.com/nanohype/portal.git", st.Repos.Portal); err != nil {
			return err
		}
	}
	// Every component this config will apply must have a live root in the tree just
	// cloned. This is the first phase at which a tree exists to check, and the last
	// before anything is provisioned.
	return assertComponentRoots(st)
}

// --- Phase 2: identity & state backend ---
type identity struct{ base }

func (identity) Run(ctx context.Context, st *engine.State) error {
	st.Runner.Dir = st.Repos.LandingZone
	c := st.Config
	note(st, "creating the versioned, encrypted, public-access-blocked S3 tfstate backend %s-%s-tfstate",
		c.Cloud.AccountID, c.Cloud.Region)
	return st.Runner.Run(ctx, "scripts/init-backend-aws.sh", c.Cloud.AccountID, c.Cloud.Region)
}

// --- Phase 3: network & cluster ---
type cluster struct{ base }

func (cluster) Run(ctx context.Context, st *engine.State) error {
	st.Runner.Dir = st.Repos.LandingZone

	// Both components' per-run inputs are supplied by componentEnv, scoped to the one
	// invocation that declares them: the create-mode levers to `network`, and cluster_name
	// plus the endpoint posture to `cluster`. landing-zone's committed cluster tree is
	// private-by-default and fail-closed — a public API endpoint with no allow-list is
	// rejected at plan time — and rackctl owns that fragile input, auto-detecting the
	// operator's egress IP when the allow-list is empty. Config validation has already
	// rejected any contradictory network combination.
	//
	// This phase deliberately sets nothing on st.Runner.Env. It used to, and those variables
	// then rode into every phase after it; see the comment on apply().
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
// managed-monitoring, dns, model-import) can only ever be applied in the one order
// CoreComponents documents — restating the list is what let three of them silently go
// missing once.
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

	// Say exactly what model-import provisions, and — more importantly — what it does
	// not. It is easy to read "model import is on" as "an open-weight model will work",
	// and three separate things stand between here and that. The import itself is an
	// out-of-band human act. landing-zone's agent-iam expands a tenant's Bedrock grant
	// only to foundation-model and inference-profile ARNs, and its own variable doc says
	// custom/imported models are not matched. And independently, the operator's
	// bedrock-model-scoping inline policy is a Deny whose NotResource is that same
	// expanded list, so it excludes an imported-model ARN even if the baseline allowed
	// it. An `imported` ModelGateway route therefore gets AccessDenied at InvokeModel
	// until BOTH repos change — which is why the note names both rather than implying
	// one upstream fix is enough.
	if st.Config.AgentPlatform.ModelImport {
		note(st, "model-import: provisioning the Bedrock Custom Model Import substrate for %s in account %s / %s — "+
			"the S3 staging bucket %s-%s-%s-model-import, the import service role model-import-%s-%s, and the SSM "+
			"discovery parameters /eks-agent-platform/%s/model-import/{staging_bucket_name,import_role_arn}. It imports "+
			"NO model: that "+
			"is a deliberate out-of-band step (eks-agent-platform/docs/runbooks/import-open-weight-model.md). Nor does a "+
			"tenant reach an imported model yet — landing-zone's agent-iam baseline cannot express an imported-model ARN, "+
			"and the operator's own bedrock-model-scoping policy denies everything outside the foundation-model and "+
			"inference-profile ARNs it expands, so an imported route needs a coordinated change in both repos",
			st.Config.Environment, st.Config.Cloud.AccountID, st.Config.Cloud.Region,
			st.Config.Environment, st.Config.Cloud.AccountID, st.Config.Cloud.Region,
			st.Config.Environment, st.Config.Cloud.Region,
			st.Config.Environment)
	}

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
		note(st, "account-id writeback: (apply) scans eks-gitops/addons/**/values-%s.yaml for %s placeholders and, "+
			"if any are found, commits & pushes the fork. The current catalog carries none — it is public, so addons "+
			"bind their IAM roles through EKS Pod Identity and the one ApplicationSet that needs a role ARN reads it "+
			"from an annotation on the ArgoCD cluster Secret", env, gitops.Placeholder)
		return nil
	}
	note(st, "account-id writeback: scanning eks-gitops/addons/**/values-%s.yaml", env)
	n, changed, err := gitops.WriteBack(st.Repos.EKSGitops, env, st.Config.Cloud.AccountID)
	if err != nil {
		return err
	}
	// Zero matches is the NORMAL case against the current catalog, and saying "replaced 0
	// placeholder(s)" invites the reader to think something went wrong. Nothing did: the
	// catalog is public, so it commits no account id at all. Almost every addon binds its
	// IAM role through EKS Pod Identity, which needs no ARN in the values; the one that
	// does need an ARN reads it from an annotation cluster-bootstrap stamps on the ArgoCD
	// cluster Secret. The writeback substitutes into whatever DOES carry a placeholder,
	// which against this catalog is nothing.
	if n == 0 {
		note(st, "no %s placeholders in the fork — the current catalog commits no account id: addons bind their "+
			"IAM roles through EKS Pod Identity, and the one ApplicationSet that needs a role ARN templates it from "+
			"an annotation cluster-bootstrap stamps on the ArgoCD cluster Secret. Nothing to write back, nothing to "+
			"push", gitops.Placeholder)
	} else {
		note(st, "replaced %d placeholder(s) across %d file(s)", n, len(changed))
	}
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
	for i := len(comps) - 1; i >= 0; i-- { // reverse of apply, no exceptions
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

	// Wait on the health STATUS, not on a condition. ArgoCD publishes no `Healthy`
	// condition — `ApplicationConditionType` carries only error and warning types
	// (InvalidSpecError, ComparisonError, SyncError, SharedResourceWarning,
	// OrphanedResourceWarning, …). Health lives at `.status.health.status`, which is
	// exactly what the diagnostic below already reads.
	//
	// So `--for=condition=Healthy` could never be satisfied: every install, including a
	// perfect one, burned the full 30 minutes and then failed. That is the worst shape a
	// failure can take — the platform is up and converged, and the tool that built it
	// reports otherwise after half an hour of silence.
	//
	// This is the same bug as phase 9's `--for=condition=Ready` on a Platform, and it was
	// written twice in one file. `kubectl wait --for=condition=X` is only ever valid when
	// something actually writes a condition of type X; a status field that happens to be
	// spelled like a condition is not one. The other two waits in this phase pipeline are
	// fine because they name real built-in conditions — `Established` on a CRD and
	// `Available` on a Deployment are both written by Kubernetes itself.
	note(st, "waiting for ArgoCD applications to converge (sync-waves 0→52)")
	if err := st.Runner.Run(ctx, "kubectl", "-n", "argocd", "wait",
		"--for=jsonpath={.status.health.status}=Healthy",
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

// tenantControlPlaneNamespace is where charts/tenant plants a tenant's control-plane CRs.
//
// It is the chart's `controlPlaneNamespace` VALUE that decides this — not the Helm
// release namespace. The Platform, BudgetPolicy, ModelGateway, AgentFleet and EvalSuite
// templates all set `namespace: {{ .Values.controlPlaneNamespace | default
// .Release.Namespace }}`, and the value defaults to eks-agent-platform. So rackctl
// passes the same string as `--namespace` AND as `--set controlPlaneNamespace=`: pinning
// only one leaves the other free to drift, and the two disagreeing is precisely how a
// namespace-blind `kubectl wait` ends up looking for a Platform in the kubeconfig's
// current namespace and failing NotFound against a tenant that is up and healthy.
const tenantControlPlaneNamespace = "eks-agent-platform"

// smoke vends the first tenant from eks-agent-platform's charts/tenant and waits for the
// operator to reconcile it. It is the end-to-end proof that the platform can actually
// take a tenant, which is the thing every earlier phase was building toward.
//
// Two traps live in these three lines, and this phase fell into both.
//
// The chart's values are nested under `platform.` — platform.name, platform.tenant,
// platform.persona. rackctl used to pass bare `tenant=` and `persona=`, and never passed
// platform.name at all. Helm accepts unknown --set paths silently, so those became three
// orphan values no template reads, and the render died on the chart's own
// `fail "platform.name is required"` guard before a single object was created. Silence is
// why it survived: --set on a path nothing reads produces no warning, and the phase is
// opt-in, so an install that never enabled a firstTenant looked entirely healthy.
//
// And a Platform never gets a `Ready` CONDITION. The operator reports readiness as
// status.phase == "Ready" (that is what the CRD's printcolumn reads); the only conditions
// it ever writes are Suspended, ModelAccessScoped, NamespaceReady and VClusterReady.
// `kubectl wait --for=condition=Ready` therefore cannot ever be satisfied — it blocks for
// the entire 15-minute timeout and then fails the phase against a tenant that came up
// fine, which is the worst possible shape for a failure: the platform works, and the tool
// that just built it says it does not.
type smoke struct{ base }

func (smoke) Run(ctx context.Context, st *engine.State) error {
	ft := st.Config.FirstTenant
	ns := tenantControlPlaneNamespace
	st.Runner.Dir = st.Repos.AgentPlatform

	note(st, "vending first tenant %q (tenant=%s persona=%s, $%d/mo) from charts/tenant into %s",
		ft.Name, ft.Tenant, ft.Persona, ft.MonthlyBudgetUSD, ns)
	note(st, "this proves the vending + identity path only: charts/tenant emits displayName/persona/tenant/"+
		"isolation/budget/identity.{allowedModelFamilies,extraPolicyArns}/compliance, and cannot express "+
		"spec.datastores, spec.identity.capabilities or spec.identity.directSecretReads — so the tenant's AWS "+
		"datastore and capability grants are not exercised here")

	// The config's model boundary and compliance flags must reach the tenant, not just
	// the operator's IAM. charts/tenant exposes exactly the matching paths, and passing
	// neither would vend the first tenant against the chart's own defaults — so a config
	// declaring `hipaa: true` or a narrowed model family would produce a Platform that
	// silently contradicts it. Rendering a tenant that disagrees with the config that
	// asked for it is the failure class this repo exists to kill, and it is worse here
	// than anywhere: phase 9 is the run's proof that vending works.
	args := []string{"upgrade", "--install", ft.Name, "charts/tenant",
		"--namespace", ns,
		"--set", "controlPlaneNamespace=" + ns,
		"--set", "platform.name=" + ft.Name,
		"--set", "platform.tenant=" + ft.Tenant,
		"--set", "platform.persona=" + ft.Persona,
		"--set", fmt.Sprintf("budget.monthlyUsd=%d", ft.MonthlyBudgetUSD),
		"--set", fmt.Sprintf("platform.compliance.soc2=%t", st.Config.AgentPlatform.Compliance.SOC2),
		"--set", fmt.Sprintf("platform.compliance.hipaa=%t", st.Config.AgentPlatform.Compliance.HIPAA),
	}
	// helm's --set parses commas as list separators, so a families list rides the
	// {a,b} literal form rather than repeated --set calls.
	if fams := st.Config.AgentPlatform.BedrockModelFamilies; len(fams) > 0 {
		args = append(args, "--set", "identity.allowedModelFamilies={"+strings.Join(fams, ",")+"}")
	}
	if err := st.Runner.Run(ctx, "helm", args...); err != nil {
		return err
	}
	return st.Runner.Run(ctx, "kubectl", "-n", ns, "wait",
		"--for=jsonpath={.status.phase}=Ready", "platform/"+ft.Name, "--timeout=15m")
}

// Teardown removes the tenant BEFORE the substrate underneath it goes away.
//
// This is load-bearing, not tidiness. The engine tears completed phases down in reverse,
// so deleting the Platform here happens while the operator is still running — which is
// the only time its finalizer can drop the per-tenant IAM role. If that role survives,
// the substrate phase's `terragrunt destroy` of agent-iam fails on DeleteConflict trying
// to delete the tenant baseline policy the role attaches, and a teardown that cannot run
// is how a half-built platform stays billing. Mirrors portal.Teardown.
func (smoke) Teardown(ctx context.Context, st *engine.State) error {
	return st.Runner.Run(ctx, "helm", "uninstall", st.Config.FirstTenant.Name,
		"-n", tenantControlPlaneNamespace, "--ignore-not-found")
}
