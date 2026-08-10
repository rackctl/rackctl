// Package phases implements the ordered 0→running bootstrap. Each phase
// orchestrates the existing nanohype repos — landing-zone (Terragrunt),
// eks-gitops (ArgoCD catalog), eks-agent-platform (operator). rackctl is the
// glue that automates landing-zone/docs/first-deploy-aws.md, NOT a rewrite.
package phases

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

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
//     applied BEFORE cluster-bootstrap or the read fails. It is gated on the FULL
//     observability tier, and that gate is the whole invariant: tier=full means the
//     full-tier OTel gateway will mount AMP_REMOTE_WRITE_URL from a Secret that
//     external-secrets syncs out of AWS Secrets Manager, and this component is the only thing
//     that writes that entry. Nothing in Terraform can check that cross-root fact — rackctl
//     is the only place it can be held.
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
	if cfg.FullObservability() {
		comps = append(comps, "managed-monitoring") // must precede cluster-bootstrap
	}
	// observability is NOT conditional, and it is gated on nothing: it publishes the three
	// /eks-agent-platform/<cluster>/observability/alerts_{critical,warning,info}_topic_arn
	// parameters unconditionally, and it is their sole producer. rackctl applied it nowhere at
	// all until now, so every rackctl-installed cluster had consumers of that contract and no
	// producer.
	//
	// Its POSITION here is currently arbitrary — no component rackctl applies reads those
	// parameters, so only the phase boundary (substrate before gitops) is load-bearing. It is
	// placed early because the consumer that WILL bind is eks-agent-platform's kill-switch,
	// whose burn-rate rules resolve both topic ARNs through unguarded `data` blocks at PLAN
	// time. rackctl does not apply that tree yet; when it does, this ordering stops being
	// arbitrary and starts being required.
	comps = append(comps, "observability")
	// druid is the per-tenant analytics substrate (Aurora Serverless + optionally MSK), gated
	// because it is real money and most platforms never need it. Its live leaf is
	// self-sufficient — it carries its own `tenants` sizing map — and it depends on network and
	// cluster, both of which the cluster phase applied before this one. Live roots exist under
	// every workload environment. Teardown: development always; elsewhere via
	// force_destroy_buckets (O1 settled upstream; target 11 wires the two-act apply).
	if cfg.Addons.Druid {
		comps = append(comps, "druid")
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
	// fleet-hub is the eks-fleet control plane's AWS half: the eks-fleet-crossplane
	// role provider-opentofu assumes, its permissions boundary, and the state bucket
	// every vended cluster's tofu state lands in. It belongs in this list rather than
	// inside the fleet phase because being here buys three things the fleet phase
	// cannot: acquire asserts its live root exists before a dollar is spent, the
	// substrate teardown destroys it in reverse with everything else, and it is applied
	// once, early, rather than at the point Crossplane is already installing.
	//
	// It depends only on the cluster component, which the cluster phase applied before
	// this one, so its position here is free.
	if cfg.ControlPlane.EKSFleet {
		comps = append(comps, "fleet-hub")
	}
	// portal is a Platform tenant of the platform it operates, so its Postgres, Redis and
	// object store come from tenant-substrate like any other tenant's. Same three reasons
	// fleet-hub is here rather than inside its phase: acquire asserts the live root before
	// a dollar is spent, the substrate teardown destroys it in reverse, and it is applied
	// once, early, rather than while helm is already installing.
	//
	// It is handed a tenants map containing portal ALONE — see portalTenantsVar. The
	// environment's committed map is every tenant APP it carries, and provisioning four
	// unrelated Aurora clusters on the way to standing up a console is not this command's
	// job.
	if cfg.ControlPlane.Portal {
		comps = append(comps, "tenant-substrate")
	}
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
	env, err := componentEnv(ctx, st, component, "apply")
	if err != nil {
		return err
	}
	return tg(ctx, st, "apply", component, env...)
}

func destroy(ctx context.Context, st *engine.State, component string) error {
	env, err := componentEnv(ctx, st, component, "destroy")
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
// The verb matters only for what gets PRINTED. Every variable below is injected on both
// paths — a destroy plan needs the same inputs the apply used — but a builder that prints
// "do this next" is describing an apply, and printing that under `destroy dns` told the
// operator to point a domain's NS records at name servers the same run was deleting. Notes
// that merely state which value is being sent stay on both paths, because they are true on
// both.
func componentEnv(ctx context.Context, st *engine.State, component, verb string) ([]string, error) {
	switch component {
	case "tenant-substrate":
		// portal only — the committed map is the GitOps concern.
		blob, err := portalTenantsVar(st)
		if err != nil {
			return nil, err
		}
		note(st, "tenant-substrate: TF_VAR_tenants carries portal and nothing else. The leaf reads "+
			"tenants.generated.json, which is every tenant APP this environment carries — an "+
			"installer applying that would provision their databases too")
		return []string{"TF_VAR_tenants=" + blob}, nil
	case "network":
		// Mode, create-mode levers (IPAM/TGW/egress), adopt inputs, and the sizing knobs
		// (vpc_cidr, nat_gateways) when they differ from Default().
		return clusterNetworkEnv(st, verb), nil
	case "cluster":
		// cluster_name, endpoint posture, and sizing (version + system nodes) when non-default.
		// The endpoint builder may detect this host's egress IP, so it can fail and must be
		// able to say so.
		return clusterEnv(ctx, st)
	case "dns":
		// domain_name + acm_certificates + enable_dnssec. All three leaf-pinned to values that
		// fail or mislead, and two of them are what make a dns-enabled install fail after
		// building a cluster. See dns.go.
		return dnsEnv(st, verb)
	case "agent-iam":
		// bedrock_allowed_model_ids from agentPlatform.bedrockModelFamilies, only when the
		// mapped globs differ from Default() — otherwise the leaf's fleet default stands.
		return agentIAMEnv(st), nil
	case "cluster-bootstrap":
		// tenants_repo_url when the portal's tenants repo is set, plus the GITHUB_TOKEN
		// that variable makes mandatory — setting it arms the component's github provider.
		// The enable_* booleans live in tgEnv (global), because cluster-bootstrap is not
		// the only consumer of some of them and the ambient injection is deliberate.
		return clusterBootstrapEnv(ctx, st)
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
func cloneOrUpdate(ctx context.Context, st *engine.State, url, dir, ref string) error {
	name := filepath.Base(dir)
	fresh := false
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if err := st.Runner.Run(ctx, "git", "clone", url, dir); err != nil {
			return err
		}
		fresh = true
	}
	prev := st.Runner.Dir
	st.Runner.Dir = dir
	defer func() { st.Runner.Dir = prev }()

	if ref == "" {
		if fresh {
			return nil
		}
		if err := st.Runner.Run(ctx, "git", "pull", "--ff-only"); err != nil {
			note(st, "%s: could not fast-forward — it has diverged from upstream, and this run "+
				"will use the code as it stands on disk", name)
			return nil
		}
		note(st, "%s updated to latest", name)
		return nil
	}

	// Pinned. Fetch tags explicitly — a plain fetch does not bring tags that are not
	// on a fetched branch, and a release tag frequently is not.
	if !fresh {
		if err := st.Runner.Run(ctx, "git", "fetch", "--tags", "--prune", "origin"); err != nil {
			return fmt.Errorf("fetching %s to resolve the pin %q: %w", name, ref, err)
		}
	}

	// Resolve BEFORE checking out, so an unknown ref is reported as an unknown ref
	// rather than as whatever git does next. This is the whole reason pinning is worth
	// having: a typo that silently left the checkout on the default branch would build
	// a platform from HEAD while the config, the logs and the operator all said
	// otherwise — the exact failure a pin exists to remove.
	if _, err := st.Runner.Capture(ctx, "git", "rev-parse", "--verify", ref+"^{commit}"); err != nil {
		return &engine.NoRollbackError{Err: fmt.Errorf(
			"%s has no ref %q. versions pins this repo to that ref, and rackctl will not fall "+
				"back to the default branch — a pin that silently tracks HEAD is worse than no "+
				"pin. Check the tag exists on the remote, or remove the entry from versions", name, ref)}
	}
	if err := st.Runner.Run(ctx, "git", "-c", "advice.detachedHead=false", "checkout", "--force", ref); err != nil {
		return err
	}
	sha, _ := st.Runner.Capture(ctx, "git", "rev-parse", "--short", "HEAD")
	note(st, "%s pinned to %s (%s)", name, ref, strings.TrimSpace(sha))
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
// An org that PUBLISHES the upstream catalog has no fork, and both branches below are
// wrong for it. `gh repo view nanohype/eks-gitops` succeeds — it is upstream — so the
// sync branch is taken and asks GitHub to fast-forward main onto itself. That fails, and
// the failure is reported as "it has diverged from nanohype/eks-gitops", which is a
// frightening and completely false statement about the canonical catalog. The fork branch
// is no better: GitHub refuses to fork a repo into the account that owns it.
//
// Neither is fatal, which is exactly why it is worth naming. The run continues against a
// catalog that is correct, having just told the operator it is diverged — and "the tool
// says something alarming that turns out to mean nothing" is how a real divergence
// warning stops being read.
func forkOrSync(ctx context.Context, st *engine.State, org string) error {
	// A pinned catalog must not be fast-forwarded. `gh repo sync` moves the fork's main
	// to upstream's, and the local checkout is then rewound to the pin — so the fork
	// ArgoCD reads and the tree rackctl reads would be different code, which is exactly
	// the split the pin exists to prevent.
	if st.Config.Versions.EKSGitops != "" {
		note(st, "versions.eksGitops pins the catalog to %s — not syncing the fork, because "+
			"fast-forwarding it would move what ArgoCD reads off the pin",
			st.Config.Versions.EKSGitops)
		return nil
	}
	if engine.OwnsUpstreamCatalog(org) {
		note(st, "%s publishes %s — there is no fork: the org's catalog IS upstream, and ArgoCD "+
			"syncs it directly", org, engine.UpstreamCatalog)
		return nil
	}

	fork := org + "/eks-gitops"

	if _, err := st.Runner.Capture(ctx, "gh", "repo", "view", fork, "--json", "name"); err != nil || st.Runner.DryRun {
		note(st, "forking %s → %s (ArgoCD syncs the catalog from the org's fork, never upstream)",
			engine.UpstreamCatalog, fork)
		return st.Runner.Run(ctx, "gh", "repo", "fork", engine.UpstreamCatalog,
			"--org", org, "--fork-name", "eks-gitops", "--clone=false")
	}

	note(st, "%s already exists — syncing it with upstream", fork)
	if err := st.Runner.Run(ctx, "gh", "repo", "sync", fork,
		"--source", engine.UpstreamCatalog, "--branch", "main"); err != nil {
		note(st, "%s: could not fast-forward — it has diverged from %s, and this "+
			"run will use the catalog as it stands on the fork. If that is not intended, reconcile "+
			"the fork before re-running.", fork, engine.UpstreamCatalog)
	}
	return nil
}

func (acquire) Run(ctx context.Context, st *engine.State) error {
	org := st.Config.Org.Name
	v := st.Config.Versions
	st.Repos = engine.RepoPaths(org)
	note(st, "cloning platform repos into %s", st.Repos.Workdir)
	if v.Any() {
		note(st, "versions: pinned refs are checked out and NEVER fast-forwarded; an unknown ref "+
			"stops the run rather than falling back to the default branch")
	}
	if err := cloneOrUpdate(ctx, st, "https://github.com/nanohype/landing-zone.git", st.Repos.LandingZone, v.LandingZone); err != nil {
		return err
	}
	if err := cloneOrUpdate(ctx, st, "https://github.com/nanohype/eks-agent-platform.git", st.Repos.AgentPlatform, v.EKSAgentPlatform); err != nil {
		return err
	}
	if err := forkOrSync(ctx, st, org); err != nil {
		return err
	}
	// Clone the fork to the exact path (gh's --clone ignores the target dir).
	if err := cloneOrUpdate(ctx, st,
		fmt.Sprintf("https://github.com/%s/eks-gitops.git", org), st.Repos.EKSGitops, v.EKSGitops); err != nil {
		return err
	}
	// The portal chart is not published to ghcr; clone its repo so the portal
	// phase can install from the local chart (mirrors the operator fallback).
	if st.Config.ControlPlane.Portal {
		note(st, "cloning nanohype/portal (day-2 UI) for its local chart")
		if err := cloneOrUpdate(ctx, st, "https://github.com/nanohype/portal.git", st.Repos.Portal, v.Portal); err != nil {
			return err
		}
	}
	// eks-fleet is the cluster control plane (Crossplane composition + Cluster XRD).
	// The fleet phase runs entirely against this checkout — it is a separate repo from
	// landing-zone, and the old phase applied a path that never existed under the
	// landing-zone Dir. Clone only when gated on, mirroring the portal.
	if st.Config.ControlPlane.EKSFleet {
		note(st, "controlPlane.eksFleet is on — cloning nanohype/eks-fleet for the hub control-plane install")
		if err := cloneOrUpdate(ctx, st, "https://github.com/nanohype/eks-fleet.git", st.Repos.EKSFleet, v.EKSFleet); err != nil {
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
	// Both components' outputs, not just the cluster's. The agent-platform substrate (phase 6)
	// needs vpc_id, private_subnet_ids and the two route-table lists from `network`, and there
	// is no SSM fallback for any of them — the network component publishes no SSM parameters at
	// all, so a terragrunt output read here is the only channel.
	captureOutputs(ctx, st, "network")
	captureOutputs(ctx, st, "cluster")
	if err := st.Runner.Run(ctx, "aws", "eks", "update-kubeconfig", "--name", st.Config.ClusterName()); err != nil {
		return err
	}
	// The kubeconfig now points at the cluster this run built. Recording that is what
	// permits the rollback's reap sweep to run at all — see engine.State.KubeconfigCluster.
	// It is set after the command rather than before it so a failed repoint leaves the
	// sweep disabled, which is the correct posture: kubectl still resolves the operator's
	// previous context, and the sweep deletes everything it can see there.
	st.KubeconfigCluster = st.Config.ClusterName()
	return nil
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

	// Disclose the tier, because rackctl injects it over whatever the leaf pinned and every
	// other lever that does that prints what it would land (see network.go). floor gets the
	// louder line: it is the one value that can PRUNE a running cluster's telemetry, since
	// re-running init is normal and an ambient TF_VAR wins over the leaf.
	if st.Config.FullObservability() {
		note(st, "observability tier: full — applying managed-monitoring (AMP + AMG) and labelling the cluster "+
			"observability/tier=full, which is what the in-cluster LGTM stack and the AMP remote-write Secret "+
			"select on")
	} else {
		note(st, "observability tier: FLOOR — managed-monitoring is NOT applied. Metrics go to CloudWatch as EMF, "+
			"logs to CloudWatch Logs, traces to X-Ray. On a cluster that previously ran full this PRUNES Loki, "+
			"Tempo, grafana-operator and the dashboards, with a telemetry gap while it converges — see "+
			"eks-gitops/docs/runbooks/observability-tier.md")
	}

	// druid is real money and opt-in. landing-zone now sets skip_final_snapshot /
	// final_snapshot_identifier on Aurora and force_destroy on the per-tenant buckets
	// (development always; elsewhere via force_destroy_buckets — the two-act contract
	// target 11 wires). Development tears down cleanly; staging/production need that
	// flag applied before destroy.
	if st.Config.Addons.Druid {
		note(st, "addons.druid: true — applying the per-tenant analytics substrate (Aurora Serverless, "+
			"optionally MSK). Development tears down cleanly; outside development a destroy needs "+
			"force_destroy_buckets applied first (rackctl destroy --force-buckets once target 11 lands)")
	}

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
		// The secrets CMK is the one landing-zone value the agent-platform substrate cannot get
		// any other way: it is a single key serving BOTH of that tree's kms variables, and the
		// network component publishes no SSM parameters at all, so a terragrunt output read is
		// the only channel for either.
		if comp == "secrets" {
			captureOutputs(ctx, st, "secrets")
		}
		// The zone exists now, so its name servers are readable — and the certificate this
		// component just requested cannot validate until something points at them.
		if comp == "dns" {
			delegateSubdomain(ctx, st)
		}
		// The endpoints and the RDS master-secret ARN portal's chart needs. There is no
		// other channel: RDS names that secret itself, so it cannot be composed.
		if comp == "tenant-substrate" {
			captureOutputs(ctx, st, "tenant-substrate")
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
	// Before the zone goes, take back the delegation pointing at it. Afterwards the child
	// zone is unreadable and the record cannot be matched for deletion.
	WithdrawSubdomainDelegation(ctx, st)

	// Every component is attempted. Returning on the first failure is the same defect
	// `rackctl destroy` had in three places, and it is worse here: a rollback runs on a
	// platform the operator did NOT ask to keep, and the expensive components are last in
	// this order.
	//
	// fleet-hub makes it concrete and permanent. Its state bucket and CMK carry
	// prevent_destroy — deliberately, they hold every vended cluster's tofu state — so a
	// rollback of a fleet-enabled platform ALWAYS fails there, and everything behind it
	// (dns, observability, managed-monitoring, agent-iam, secrets) was stranded every
	// time. The cluster and VPC belong to other phases, so they survived on their own
	// teardown, but the ordering guarantees the rollback stops partway on any install with
	// controlPlane.eksFleet on.
	//
	// Failures are joined and returned, so the engine still reports the rollback as
	// incomplete and names what did not come down.
	comps := substrateComponents(st.Config)
	var failed []error
	for i := len(comps) - 1; i >= 0; i-- { // reverse of apply, no exceptions
		if err := destroy(ctx, st, comps[i]); err != nil {
			failed = append(failed, fmt.Errorf("%s: %w", comps[i], err))
			note(st, "teardown %s failed — continuing with the rest", comps[i])
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d component(s) did not tear down: %w", len(failed), errors.Join(failed...))
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
		// By the time this runs, the substrate phases have built the VPC, the EKS cluster
		// and every AWS dependency the addons need. A cluster-bootstrap failure means
		// ArgoCD did not install — a GitHub 401 on the tenants-repo deploy key, a chart
		// that will not render, a webhook not yet up. None of those are a reason to
		// demolish the cloud underneath.
		//
		// The convergence wait immediately below already takes exactly this position, in
		// as many words. An apply failure in the SAME phase reaching the opposite
		// conclusion was an inconsistency, not a decision: it returned bare, so the
		// rollback sweep ran and destroyed the cluster and the VPC over a credential the
		// operator could have fixed in ten seconds and re-run.
		return &engine.NoRollbackError{Err: fmt.Errorf(
			"installing ArgoCD failed. The cloud IS provisioned and the cluster is left "+
				"standing — fix the cause and re-run `rackctl apply` (it is re-runnable), or "+
				"`rackctl destroy` if you want it gone: %w", err)}
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
	if err := waitCatalogConverged(ctx, st, 30*time.Minute); err != nil {
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
				"the cluster is left standing — run `rackctl check` to see what has not settled, and " +
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
	// The AWS substrate this operator governs with, applied BEFORE the wait below.
	//
	// rackctl applied none of it until now, and the operator is the thing that notices: it
	// reads SSM at startup for kill-switch.event_bus_name, cost-pipeline.athena_workgroup,
	// cost-pipeline.athena_database and bedrock.baseline_guardrail_id, and its budget
	// reconciler queries the Athena workgroup cost-pipeline creates. ArgoCD deployed the
	// operator one phase ago, so it has already come up without them; applying here and then
	// waiting for Available is what lets it recover on its next restart rather than being
	// declared healthy while blind.
	if err := applyAgentPlatform(ctx, st); err != nil {
		return err
	}

	// Now that eval-runtime has written its SSM parameters, republish cluster-bootstrap with
	// enable_eval_runtime on.
	//
	// This is a re-apply, and it is the only way the annotation can ever be stamped. The
	// variable is opt-in upstream BECAUSE it depends on another component having run
	// (cluster-bootstrap/variables.tf:182 — "Requires that component to have applied
	// first"), and rackctl runs cluster-bootstrap in the gitops phase, one phase before the
	// agent-platform tree exists. Setting the flag there would fail the SSM read; leaving it
	// unset means bootstrap.tf:397-400 never stamps
	// `eks-agent-platform/eval-reports-bucket` on the ArgoCD cluster Secret, the operator
	// ApplicationSet renders evalReportsBucket empty, and every EvalSuite run completes with
	// its reports going nowhere durable. Nothing errors — which is the whole problem.
	//
	// tgenv.go states this rule for enable_managed_monitoring and it applies verbatim here:
	// any landing-zone variable that is opt-in because it depends on another component
	// having run is a variable rackctl must supply. This is that same wire, one flag over.
	//
	// The apply is a near-no-op — cluster-bootstrap converged a phase ago — but it is a real
	// apply, so it is announced rather than slipped in.
	note(st, "republishing cluster-bootstrap with enable_eval_runtime=true — eval-runtime's SSM "+
		"parameters exist only now, and this is what stamps eks-agent-platform/eval-reports-bucket "+
		"on the ArgoCD cluster Secret. Without it the operator renders an empty bucket and EvalSuite "+
		"reports are never persisted")
	prevDir := st.Runner.Dir
	st.Runner.Dir = st.Repos.LandingZone
	err := applyWith(ctx, st, "cluster-bootstrap", "TF_VAR_enable_eval_runtime=true")
	st.Runner.Dir = prevDir
	if err != nil {
		return fmt.Errorf("republishing cluster-bootstrap for eval-runtime: %w", err)
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

// Teardown destroys the agent-platform AWS substrate this phase applied.
//
// The operator itself is untouched, for the reason this used to be a no-op entirely: it is an
// ArgoCD Application, so it goes when the cluster does. Uninstalling a Helm release rackctl
// no longer creates would fail, and deleting it out from under ArgoCD would just make ArgoCD
// put it back.
//
// The terraform tree is different — rackctl applies it directly, so rackctl owns unwinding
// it, and it has to happen HERE rather than later in the rollback. Reverse-phase order puts
// this before the substrate and cluster phases, which is exactly the constraint the tree
// needs: its components resolve landing-zone's SSM parameters through unguarded data blocks
// that a destroy plan still evaluates.
// The account-scoped roots are deliberately left standing, and a rollback is the strongest case
// for that default rather than the weakest. This path is not an operator's decision — it is
// automatic cleanup after a phase failed — and the two roots under live/org hold the account's
// only Bedrock invocation-logging configuration and its only cost pipeline, shared by every
// environment installed here. A failed development apply that swept them would take production's
// invocation logging with it, unattended, with nothing red.
//
// They are named at apply time and again here, so what survives a rollback is stated rather than
// discovered. Removing them is `rackctl destroy --account-scoped`, which is a sentence somebody
// has to type.
func (platform) Teardown(ctx context.Context, st *engine.State) error {
	if !st.Config.AgentPlatform.Enabled() {
		return nil
	}
	return DestroyAgentPlatform(ctx, st, AgentPlatformTeardown{})
}

// --- Phase 7 (optional): eks-fleet cluster control plane ---
//
// Turns the cluster this run just stood up into a hub that can vend further
// clusters via namespaced Cluster CRs. The real sequence lives in
// eks-fleet/docs/stand-up-the-hub.md §4 and config/bootstrap/README.md — not a
// single `kubectl apply -f crossplane.yaml` (that file is a Configuration
// package meta, not an installable set of resources, and the old phase pointed
// Runner.Dir at landing-zone, which has no eks-fleet/ directory at all).
//
// What this phase does NOT do, and why:
//
//   - It does not apply landing-zone's fleet-hub component. That lives under
//     live/aws/fleet/… (a dedicated fleet account), and componentDir can only
//     address live/aws/workload-<env>/…. Same boundary as the multi-account
//     seam (ledger S10): rackctl reaches the workload tree, not the fleet tree.
//     The IRSA role + nanohype-eks-fleet-tfstate bucket that fleet-hub mints are
//     a prerequisite the operator supplies (or that a future rung-0 campaign
//     vends); without them provider-opentofu has no ambient credentials.
//   - It does not write Cluster CRs into org.gitops.clustersRepo. That is the
//     day-2 vending surface; this phase only installs the factory.
//
// Order matches the bootstrap README: Crossplane → provider+functions →
// ProviderConfig → XRD + composition → reaper.
type fleet struct{ base }

// crossplaneHelmVersion is the pin stand-up-the-hub.md installs. Bump with that doc.
const crossplaneHelmVersion = "2.3.1"

func (fleet) Run(ctx context.Context, st *engine.State) error {
	note(st, "installing the eks-fleet cluster control plane on this cluster — Crossplane v2, "+
		"provider-opentofu, the Cluster XRD and composition. Future clusters become namespaced "+
		"Cluster CRs (org.gitops.clustersRepo=%s is where day-2 vends land, not this phase)",
		st.Config.Org.GitOps.ClustersRepo)
	if st.Config.ControlPlane.FleetHubRoleARN == "" {
		note(st, "hub identity: %s, from the fleet-hub component the substrate phase applied into "+
			"this account. Its state bucket (nanohype-eks-fleet-tfstate) and its CMK carry "+
			"prevent_destroy — they hold every vended cluster's tofu state, so a destroy of this "+
			"platform leaves them standing on purpose", st.Config.FleetHubRoleARN())
	} else {
		note(st, "hub identity: %s, configured — this platform vends through a hub it does not own, "+
			"so nothing here creates or destroys that role", st.Config.FleetHubRoleARN())
	}

	// Everything below is relative to the eks-fleet checkout. acquire cloned it when
	// the gate was on; a missing Dir is a programming error, not an operator one.
	if st.Repos.EKSFleet == "" {
		return fmt.Errorf("controlPlane.eksFleet is on but Repos.EKSFleet is empty — acquire must clone nanohype/eks-fleet first")
	}
	st.Runner.Dir = st.Repos.EKSFleet
	note(st, "running from %s — every -f path below is relative to that checkout", st.Repos.EKSFleet)

	// 1. Crossplane v2.
	if err := st.Runner.Run(ctx, "helm", "repo", "add", "crossplane-stable", "https://charts.crossplane.io/stable"); err != nil {
		// helm repo add fails if the repo already exists; treat that as fine and continue.
		// A real network/auth failure will surface on the install below.
		note(st, "helm repo add crossplane-stable: %v (continuing — install will fail if the chart is unreachable)", err)
	}
	if err := st.Runner.Run(ctx, "helm", "upgrade", "--install", "crossplane", "crossplane-stable/crossplane",
		"-n", "crossplane-system", "--create-namespace", "--version", crossplaneHelmVersion); err != nil {
		return err
	}
	if err := st.Runner.Run(ctx, "kubectl", "-n", "crossplane-system", "rollout", "status", "deploy/crossplane", "--timeout=180s"); err != nil {
		return err
	}

	// 2. provider-opentofu + functions. providers.yaml carries a placeholder
	// eks.amazonaws.com/role-arn; the operator (or a prior fleet-hub apply) must have
	// put the real hub role there, or IRSA will not attach. We apply the file as-is and
	// say so — rewriting it would invent a role ARN we cannot see from the workload tree.
	rendered, err := renderFleetProviders(st)
	if err != nil {
		return err
	}
	if err := st.Runner.Run(ctx, "kubectl", "apply", "-f", rendered); err != nil {
		return err
	}
	if err := st.Runner.Run(ctx, "kubectl", "apply", "-f", "config/functions.yaml"); err != nil {
		return err
	}
	if err := st.Runner.Run(ctx, "kubectl", "-n", "crossplane-system", "wait",
		"--for=condition=Healthy", "provider/provider-opentofu", "--timeout=300s"); err != nil {
		return err
	}

	// 3. Single ClusterProviderConfig (credentials source None → ambient IRSA).
	if err := st.Runner.Run(ctx, "kubectl", "apply", "-f", "config/providers/providerconfig.yaml"); err != nil {
		return err
	}

	// 4. The Cluster API. Namespace create is best-effort — AlreadyExists is fine.
	_ = st.Runner.Run(ctx, "kubectl", "create", "namespace", "platform")
	if err := st.Runner.Run(ctx, "kubectl", "apply", "-f", "apis/cluster/definition.yaml"); err != nil {
		return err
	}
	if err := st.Runner.Run(ctx, "kubectl", "apply", "-f", "compositions/cluster-aws.yaml"); err != nil {
		return err
	}

	// 5. Ephemeral-spoke reaper (ttlDays > 0 Cluster CRs).
	if err := st.Runner.Run(ctx, "kubectl", "apply", "-f", "config/reaper.yaml"); err != nil {
		return err
	}

	note(st, "eks-fleet control plane installed. Validate: kubectl get xrd clusters.fleet.nanohype.dev && "+
		"kubectl get composition cluster-aws. Vend a spoke with a namespaced Cluster CR — see "+
		"eks-fleet/examples/")
	return nil
}

func (fleet) Teardown(ctx context.Context, st *engine.State) error {
	// Refuse if this hub has vended spokes, for the same reason `rackctl destroy` refuses
	// (cmd/misc.go): deleting the XRD deletes the Cluster CRs, and uninstalling Crossplane
	// removes the only control plane that could ever tear the real spoke clusters down. Those
	// are EKS control planes, VPCs and NAT gateways, frequently in other AWS accounts, tracked
	// nowhere but here. Stranding them is not something a rollback gets to do quietly, and no
	// diff of THIS account would ever show it happened.
	//
	// This path is not currently reachable — Teardown is called only from engine.teardown,
	// which runs only when a NON-optional phase fails, and every non-optional phase precedes
	// fleet, so fleet is never in `completed`. The guard is here anyway, and that is the point:
	// the same reasoning as the KubeconfigCluster gate in engine.teardown. An invariant that
	// holds because of the current phase ORDER is one accident away from not holding, and the
	// accident — adding a required phase after fleet — is invisible at the place it is made.
	// Making it structural costs one read-only query.
	if spokes := reap.FleetSpokes(ctx, st.Runner); len(spokes) > 0 {
		return fmt.Errorf(
			"refusing to tear down the eks-fleet control plane: %d spoke cluster(s) are still vended "+
				"(%s). Each is a real EKS cluster — its own control plane, VPC and NAT gateways — often "+
				"in another AWS account, and this hub is the only place they are tracked. Delete them "+
				"first with `kubectl delete clusters.fleet.nanohype.dev --all -A --wait` and let "+
				"Crossplane tear them down",
			len(spokes), strings.Join(spokes, ", "))
	}

	// Crossplane + the XRD/composition are cluster-scoped. Tear them down only if we
	// know where the checkout is; otherwise leave them — uninstalling Crossplane by
	// name alone is fine, and is what a rollback needs.
	if st.Repos.EKSFleet != "" {
		st.Runner.Dir = st.Repos.EKSFleet
		_ = st.Runner.Run(ctx, "kubectl", "delete", "-f", "config/reaper.yaml", "--ignore-not-found")
		_ = st.Runner.Run(ctx, "kubectl", "delete", "-f", "compositions/cluster-aws.yaml", "--ignore-not-found")
		_ = st.Runner.Run(ctx, "kubectl", "delete", "-f", "apis/cluster/definition.yaml", "--ignore-not-found")
		_ = st.Runner.Run(ctx, "kubectl", "delete", "-f", "config/providers/providerconfig.yaml", "--ignore-not-found")
		_ = st.Runner.Run(ctx, "kubectl", "delete", "-f", "config/functions.yaml", "--ignore-not-found")
		_ = st.Runner.Run(ctx, "kubectl", "delete", "-f", "config/bootstrap/providers.yaml", "--ignore-not-found")
	}
	return st.Runner.Run(ctx, "helm", "uninstall", "crossplane", "-n", "crossplane-system", "--ignore-not-found")
}

// --- Phase 8 (optional): operator portal ---
type portal struct{ base }

func (portal) Run(ctx context.Context, st *engine.State) error {
	// The tenant namespace the operator derives from the Platform's name. portal's pods run
	// there so its Pod Identity association binds, and so the AppProject and default-deny
	// NetworkPolicy the operator reconciles actually cover them. Installing into `default`
	// — which is what a helm command with no --namespace does — put the release outside
	// every boundary its own Platform CR declares.
	const ns = "tenants-portal"

	sub, err := readPortalSubstrate(st)
	if err != nil {
		return &engine.NoRollbackError{Err: fmt.Errorf("portal substrate: %w", err)}
	}

	// The Platform CR has to reach the cluster before the chart does, and nothing else
	// applies it. rackctl reads portal/platform.yaml for its spec.datastores — that is how
	// tenant-substrate learned to build the Aurora cluster above — but reading a file is not
	// applying it, and the eks-gitops catalog declares the platform-ops TENANT while naming
	// no Platform called portal.
	//
	// Everything the chart needs is downstream of that one object. The operator derives the
	// tenants-portal namespace from the Platform's name, creates the tenant-runtime
	// ServiceAccount in it, and binds that account to the tenant IAM role with a Pod Identity
	// association. Install the chart first and helm cheerfully creates a namespace and a set
	// of Deployments whose pods reference a ServiceAccount that does not exist — they never
	// schedule, and the release still reports deployed.
	if err := applyPortalPlatform(ctx, st, ns); err != nil {
		return &engine.NoRollbackError{Err: err}
	}

	note(st, "deploying portal (Go API + River worker + React) as a Platform tenant in %s", ns)
	note(st, "portal: database %s, credential from %s — external-secrets composes DATABASE_URL at "+
		"refresh, so the password never lands in a values file or the helm release",
		sub.DBHost, sub.DBSecretARN)

	args := []string{
		"upgrade", "--install", "portal", portalChartRef(st),
		"--namespace", ns, "--create-namespace",
		"--set", "externalSecret.enabled=true",
		"--set", "externalSecret.relationalSecretArn=" + sub.DBSecretARN,
		"--set", "tenantInfra.relational.host=" + sub.DBHost,
		"--set", "config.environment=" + portalEnvironment(st),
		"--set", "serviceAccount.name=tenant-runtime",
	}
	if sub.CacheHost != "" {
		args = append(args, "--set", "tenantInfra.cache.host="+sub.CacheHost)
		if sub.CacheSecret != "" {
			args = append(args, "--set", "externalSecret.cacheSecretArn="+sub.CacheSecret)
		}
	}
	if sub.Bucket != "" {
		args = append(args,
			"--set", "objectStore.bucket="+sub.Bucket,
			"--set", "objectStore.region="+st.Config.Cloud.Region,
			"--set", "objectStore.endpoint=s3."+st.Config.Cloud.Region+".amazonaws.com",
			"--set", "objectStore.useSSL=true",
		)
	}
	if org := st.Config.Org.Name; org != "" {
		args = append(args, "--set", "config.allowedGitHubOrg="+org)
	}
	if err := st.Runner.Run(ctx, "helm", args...); err != nil {
		return err
	}
	// helm returns once the API server accepts the objects, which is not the same as portal
	// running — and every way this phase has actually failed is invisible from there.
	if !st.Runner.DryRun {
		if err := waitPortalReady(ctx, st, ns, 10*time.Minute); err != nil {
			return &engine.NoRollbackError{Err: err}
		}
	}
	note(st, "portal: no ingress. The chart defaults to ClusterIP, and an ingress before a GitHub "+
		"OAuth app exists would publish a console whose only login is the development one. Reach it "+
		"with: kubectl -n %s port-forward svc/portal-web 8080:80", ns)
	return nil
}

// portalTenantValues are the values phase 8 sets that only the tenant-substrate wiring
// declares. They are the contract between this phase and the chart: the ExternalSecret
// that composes DATABASE_URL from the RDS-managed secret is rendered from them, and a
// chart that does not declare them cannot compose anything.
var portalTenantValues = []string{"externalSecret", "tenantInfra"}

// portalChartRef resolves the chart to install, preferring a published chart that can
// actually satisfy this phase.
//
// REACHABLE IS NOT SUFFICIENT. The published chart is a separate artifact on its own
// version line, and GHCR holding *a* portal chart says nothing about whether it holds one
// that understands `externalSecret`. Chart 0.1.0 does not: it has no externalsecret.yaml
// and declares neither value, and it carries no values.schema.json, so helm accepts every
// --set silently. What stops that install being a running portal wired to nothing is the
// chart's own `required` on database.url — a guard in the artifact being replaced, which
// is not a thing to depend on.
//
// So ask the resolved chart whether it declares the values this phase is about to set,
// and fall back to the checkout when it does not. That is the org's recurring failure
// read from the other side: a control plane reporting Healthy over a data path that was
// never connected.
//
// A PIN OUTRANKS THE REGISTRY. When versions.portal names a ref, the operator has said
// which portal this install is. Reaching past that for whatever GHCR published last
// defeats the pin exactly the way an ApplicationSet pinning its own revision and letting
// children track main does — a pinned install that quietly deploys something else.
func portalChartRef(st *engine.State) string {
	const published = "oci://ghcr.io/nanohype/portal/charts/portal"

	checkout := func(why string) string {
		note(st, "portal: installing from the checkout at %s — %s", st.Repos.Portal, why)
		st.Runner.Dir = st.Repos.Portal
		return "deploy/helm/portal"
	}

	if st.Runner.DryRun {
		return published
	}
	if ref := st.Config.Versions.Portal; ref != "" {
		return checkout(fmt.Sprintf(
			"versions.portal pins %s, and the published chart is a separate artifact that "+
				"cannot be resolved from that ref. Honouring the registry here would deploy "+
				"a portal the pin does not name", ref))
	}

	out, err := st.Runner.Capture(context.Background(), "helm", "show", "values", published)
	if err != nil {
		return checkout(fmt.Sprintf("%s is unreachable", published))
	}
	// Parsed, not grepped. `externalSecret:` appearing anywhere in a comment would
	// satisfy a line match and hand back a chart that declares nothing — the same
	// shape of false positive this function exists to remove.
	var declared map[string]any
	if err := yaml.Unmarshal([]byte(out), &declared); err != nil {
		return checkout(fmt.Sprintf("%s returned values that do not parse as YAML: %v", published, err))
	}
	var missing []string
	for _, key := range portalTenantValues {
		if _, ok := declared[key]; !ok {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return checkout(fmt.Sprintf(
			"%s declares no %s, so it renders no ExternalSecret and DATABASE_URL would never "+
				"be composed from the RDS-managed secret. The chart has no values.schema.json, "+
				"so helm would accept every --set below in silence",
			published, strings.Join(missing, " or ")))
	}
	return published
}

// portalEnvironment maps this install's environment onto the one portal validates against.
//
// portal refuses to start outside `development` without a GitHub OAuth app, an allowed
// org and a webhook secret — deliberately, because a GitHub OAuth App admits every GitHub
// account and the org check is the only boundary. rackctl has none of those to give, so a
// development install runs development. Anything else keeps its own name and fails loudly
// at startup until an operator supplies them, which is the correct outcome: a console
// admitting the internet is worse than one that will not boot.
func portalEnvironment(st *engine.State) string {
	if st.Config.Environment == config.EnvDev {
		return "development"
	}
	return string(st.Config.Environment)
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
