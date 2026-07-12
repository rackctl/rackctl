// Package phases implements the ordered 0→running bootstrap. Each phase
// orchestrates the existing nanohype repos — landing-zone (Terragrunt),
// eks-gitops (ArgoCD catalog), eks-agent-platform (operator). rackctl is the
// glue that automates landing-zone/docs/first-deploy-aws.md, NOT a rewrite.
package phases

import (
	"context"
	"fmt"

	"github.com/rackctl/rackctl/internal/engine"
	"github.com/rackctl/rackctl/internal/exec"
	"github.com/rackctl/rackctl/internal/gitops"
	"github.com/rackctl/rackctl/internal/tf"
)

// CoreComponents is the landing-zone apply order for the core path; destroy
// runs it in reverse.
var CoreComponents = []string{"network", "cluster", "secrets", "cluster-bootstrap", "cluster-addons"}

// All returns the ordered bootstrap pipeline. Phases 0–6 are the core
// 0→running path (AWS-only, v1); 7–9 are opt-in layers.
func All() []engine.Phase {
	return []engine.Phase{
		preflight{base{id: "preflight", title: "Preflight — tools, identity, quotas"}},
		acquire{base{id: "acquire", title: "Acquire platform repos (clone + fork)"}},
		identity{base{id: "identity", title: "Identity & Terraform state backend"}},
		cluster{base{id: "cluster", title: "Network & EKS cluster"}},
		bootstrap{base{id: "gitops", title: "Secrets & ArgoCD GitOps bootstrap"}},
		addons{base{id: "addons", title: "Addon convergence & IRSA writeback"}},
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
// override it (cluster, gitops, addons, platform) so the engine's rollback
// actually destroys them.
func (base) Teardown(context.Context, *engine.State) error { return nil }

func note(st *engine.State, format string, a ...any) {
	fmt.Fprintf(st.Runner.Out, "    "+format+"\n", a...)
}

// apply / destroy run a landing-zone Terragrunt component for the current env.
func apply(ctx context.Context, st *engine.State, component string) error {
	return tg(ctx, st, "apply", component)
}
func destroy(ctx context.Context, st *engine.State, component string) error {
	return tg(ctx, st, "destroy", component)
}
func tg(ctx context.Context, st *engine.State, verb, component string) error {
	dir := "live/aws/workload-" + string(st.Config.Environment) + "/" + component
	return st.Runner.Run(ctx, "terragrunt", verb, "--terragrunt-non-interactive",
		"--terragrunt-working-dir", dir)
}

// captureOutputs merges a component's `terragrunt output -json` into State. It
// is a no-op in dry-run and on any error (outputs are advisory, not required).
func captureOutputs(ctx context.Context, st *engine.State, component string) {
	if st.Runner.DryRun {
		return
	}
	dir := "live/aws/workload-" + string(st.Config.Environment) + "/" + component
	data, err := st.Runner.Capture(ctx, "terragrunt", "output", "-json", "--terragrunt-working-dir", dir)
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
		note(st, "requesting vCPU quota increase to %d", st.Config.Quotas.VCPU)
		// Ignore the error: a duplicate/pending request is expected and benign.
		_ = st.Runner.Run(ctx, "aws", "service-quotas", "request-service-quota-increase",
			"--service-code", "ec2", "--quota-code", "L-1216C47A",
			"--desired-value", fmt.Sprintf("%d", st.Config.Quotas.VCPU))
	}
	return nil
}

// --- Phase 1: acquire repos ---
type acquire struct{ base }

func (acquire) Run(ctx context.Context, st *engine.State) error {
	org := st.Config.Org.Name
	st.Repos = engine.RepoPaths(org)
	note(st, "cloning platform repos into %s", st.Repos.Workdir)
	if err := st.Runner.Run(ctx, "git", "clone", "https://github.com/nanohype/landing-zone.git", st.Repos.LandingZone); err != nil {
		return err
	}
	if err := st.Runner.Run(ctx, "git", "clone", "https://github.com/nanohype/eks-agent-platform.git", st.Repos.AgentPlatform); err != nil {
		return err
	}
	note(st, "forking nanohype/eks-gitops → %s/eks-gitops (the operator owns the addon catalog for IRSA writeback)", org)
	if err := st.Runner.Run(ctx, "gh", "repo", "fork", "nanohype/eks-gitops",
		"--org", org, "--fork-name", "eks-gitops", "--clone=false"); err != nil {
		return err
	}
	// Clone the fork to the exact path (gh's --clone ignores the target dir).
	return st.Runner.Run(ctx, "git", "clone",
		fmt.Sprintf("https://github.com/%s/eks-gitops.git", org), st.Repos.EKSGitops)
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
	note(st, "provisioning VPC then EKS control plane (network → cluster; strict ordering)")
	for _, comp := range []string{"network", "cluster"} {
		if err := apply(ctx, st, comp); err != nil {
			return err
		}
	}
	captureOutputs(ctx, st, "cluster")
	return st.Runner.Run(ctx, "aws", "eks", "update-kubeconfig", "--name", string(st.Config.Environment)+"-eks")
}

func (cluster) Teardown(ctx context.Context, st *engine.State) error {
	st.Runner.Dir = st.Repos.LandingZone
	for _, comp := range []string{"cluster", "network"} { // reverse of apply
		if err := destroy(ctx, st, comp); err != nil {
			return err
		}
	}
	return nil
}

// --- Phase 4: secrets & ArgoCD bootstrap ---
type bootstrap struct{ base }

func (bootstrap) Run(ctx context.Context, st *engine.State) error {
	st.Runner.Dir = st.Repos.LandingZone
	note(st, "installing ArgoCD + app-of-apps pointing at %s", st.Config.Org.GitOps.EKSGitopsRepo)
	for _, comp := range []string{"secrets", "cluster-bootstrap"} {
		if err := apply(ctx, st, comp); err != nil {
			return err
		}
	}
	return nil
}

func (bootstrap) Teardown(ctx context.Context, st *engine.State) error {
	st.Runner.Dir = st.Repos.LandingZone
	for _, comp := range []string{"cluster-bootstrap", "secrets"} { // reverse of apply
		if err := destroy(ctx, st, comp); err != nil {
			return err
		}
	}
	return nil
}

// --- Phase 5: addon convergence & IRSA writeback ---
type addons struct{ base }

func (addons) Run(ctx context.Context, st *engine.State) error {
	st.Runner.Dir = st.Repos.LandingZone
	if err := apply(ctx, st, "cluster-addons"); err != nil {
		return err
	}
	captureOutputs(ctx, st, "cluster-addons")

	env := string(st.Config.Environment)
	if st.Runner.DryRun {
		note(st, "FOOTGUN GUARD: (apply) substitutes the account id into eks-gitops/addons/*/values-%s.yaml, then commits & pushes the fork", env)
	} else {
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
	}

	note(st, "waiting for ArgoCD applications to converge (sync-waves 0→52)")
	return st.Runner.Run(ctx, "kubectl", "-n", "argocd", "wait", "--for=condition=Healthy",
		"applications", "--all", "--timeout=30m")
}

func (addons) Teardown(ctx context.Context, st *engine.State) error {
	st.Runner.Dir = st.Repos.LandingZone
	return destroy(ctx, st, "cluster-addons")
}

// --- Phase 6: agent-platform substrate, CRDs & operator ---
type platform struct{ base }

func (platform) Run(ctx context.Context, st *engine.State) error {
	if !st.Config.AgentPlatform.Enable {
		note(st, "agentPlatform.enable=false — skipping the agent operator")
		return nil
	}
	st.Runner.Dir = st.Repos.AgentPlatform
	note(st, "installing the agent operator + CRDs (platform/agents/governance.nanohype.dev); the operator + GitOps reconcile the substrate")
	if arn := st.Outputs["operator_role_arn"]; arn != "" {
		note(st, "operator IRSA role: %s", arn)
	}
	// The operator OCI chart is empty until the first charts-v* tag; fall back to
	// the local chart in the cloned repo when the pull fails.
	if err := st.Runner.Run(ctx, "helm", "upgrade", "--install", "operator",
		"oci://ghcr.io/nanohype/charts/operator"); err != nil {
		note(st, "operator OCI chart unavailable — falling back to local ./charts/operator")
		return st.Runner.Run(ctx, "helm", "upgrade", "--install", "operator", "charts/operator")
	}
	return nil
}

func (platform) Teardown(ctx context.Context, st *engine.State) error {
	return st.Runner.Run(ctx, "helm", "uninstall", "operator", "--ignore-not-found")
}

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
	return st.Runner.Run(ctx, "helm", "upgrade", "--install", "portal", "oci://ghcr.io/nanohype/charts/portal")
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
