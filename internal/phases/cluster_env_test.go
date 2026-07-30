package phases

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/engine"
	"github.com/rackctl/rackctl/internal/exec"
)

func sizingState(cfg *config.Config) *engine.State {
	run := exec.New(io.Discard)
	run.DryRun = true
	return &engine.State{Config: cfg, Runner: run}
}

// A default-valued config injects no sizing knobs. An ambient TF_VAR beats a leaf's
// inputs, so sending Default()'s version or node sizes would silently override whatever
// that environment carefully pinned. cluster_name and the endpoint posture always go;
// the rest only when the operator actually chose a non-default value.
func TestClusterEnv_DefaultInjectsNoSizing(t *testing.T) {
	cfg := config.Default()
	cfg.Org.Name = "acme"
	cfg.Cloud.AccountID = "111111111111"
	cfg.Cloud.Profile = "workload-development"
	cfg.Cluster.Name = "platform"
	cfg.Cluster.EndpointPublicAccess = false // avoid egress autodetection in this test
	cfg.ApplyDefaults()

	env, err := clusterEnv(context.Background(), sizingState(cfg))
	if err != nil {
		t.Fatalf("clusterEnv: %v", err)
	}

	if !slices.Contains(env, "TF_VAR_cluster_name=platform") {
		t.Fatalf("cluster_name must always be sent; got %v", env)
	}
	for _, prefix := range []string{
		"TF_VAR_cluster_version=",
		"TF_VAR_system_node_",
	} {
		for _, e := range env {
			if strings.HasPrefix(e, prefix) {
				t.Errorf("default-valued config must not inject %s — the leaf wins; got %v", prefix, env)
			}
		}
	}
}

// A non-default version is the operator's deliberate pin and must reach the leaf.
func TestClusterEnv_InjectsNonDefaultVersion(t *testing.T) {
	cfg := config.Default()
	cfg.Cluster.Name = "platform"
	cfg.Cluster.Version = "1.35"
	cfg.Cluster.EndpointPublicAccess = false

	env, err := clusterEnv(context.Background(), sizingState(cfg))
	if err != nil {
		t.Fatalf("clusterEnv: %v", err)
	}
	if !slices.Contains(env, "TF_VAR_cluster_version=1.35") {
		t.Fatalf("non-default version must be injected; got %v", env)
	}
}

// agent-iam receives bedrock_allowed_model_ids only when the mapped families differ from
// Default(). Narrowing the config without this left the IAM baseline on the full fleet
// default while the tenant CR said otherwise.
func TestAgentIAMEnv_InjectsOnlyWhenFamiliesDiffer(t *testing.T) {
	// Default families → no injection.
	cfg := config.Default()
	cfg.AgentPlatform.BedrockModelFamilies = []string{"anthropic", "amazon-nova"}
	st := sizingState(cfg)
	if env := agentIAMEnv(st); len(env) != 0 {
		t.Fatalf("default families must inject nothing (leaf default matches); got %v", env)
	}

	// Narrowed → inject.
	cfg.AgentPlatform.BedrockModelFamilies = []string{"anthropic"}
	env := agentIAMEnv(st)
	if !slices.Contains(env, `TF_VAR_bedrock_allowed_model_ids=["anthropic.*"]`) {
		t.Fatalf("narrowed families must scope the IAM baseline; got %v", env)
	}

	// Platform off → nothing, even if families are set (agent-iam is not applied).
	off := false
	cfg.AgentPlatform.Enable = &off
	if env := agentIAMEnv(st); len(env) != 0 {
		t.Fatalf("agent platform off: agent-iam is skipped, inject nothing; got %v", env)
	}
}

// tenants_repo_url rides the portal's tenants repo. Empty when unset; SSH form when set.
func TestClusterBootstrapEnv_TenantsRepo(t *testing.T) {
	st := sizingState(config.Default())
	env, err := clusterBootstrapEnv(context.Background(), st)
	if err != nil {
		t.Fatalf("clusterBootstrapEnv: %v", err)
	}
	if len(env) != 0 {
		t.Fatalf("no tenants repo: inject nothing; got %v", env)
	}

	st.Config.Org.GitOps.TenantsRepo = "github.com/acme/tenants"
	env, err = clusterBootstrapEnv(context.Background(), st)
	if err != nil {
		t.Fatalf("clusterBootstrapEnv: %v", err)
	}
	if !slices.Contains(env, "TF_VAR_tenants_repo_url=git@github.com:acme/tenants.git") {
		t.Fatalf("tenants repo must become an SSH URL for cluster-bootstrap; got %v", env)
	}
}

// ─────────────────────── the github credential ───────────────────────
//
// Setting tenants_repo_url arms cluster-bootstrap's `provider "github"`, which reads
// GITHUB_TOKEN and nothing else. Nothing used to supply it, so the apply 401'd in phase 5
// — with the VPC and the EKS cluster already built.

// tokenState returns a NON-dry-run state (the point is to exec the fake gh) whose notes
// land in the returned buffer, so a test can assert on what the operator would see.
func tokenState(t *testing.T, repo string) (*engine.State, *bytes.Buffer) {
	t.Helper()
	cfg := config.Default()
	cfg.Org.Name = "acme"
	cfg.Cluster.Name = "platform"
	cfg.Org.GitOps.TenantsRepo = repo
	cfg.ApplyDefaults()

	var out bytes.Buffer
	return &engine.State{Config: cfg, Runner: exec.New(&out)}, &out
}

// fakeGh puts a `gh` on PATH with the given body and clears any real GITHUB_TOKEN, so the
// test controls both channels the resolver reads.
func fakeGh(t *testing.T, body string) {
	t.Helper()
	t.Setenv("GITHUB_TOKEN", "")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// The documented setup path is `gh auth login`, which stores the credential in gh's
// keyring and exports NOTHING. Bridging it is what makes the documented path work.
func TestClusterBootstrapEnv_BridgesTheTokenGhIsHolding(t *testing.T) {
	fakeGh(t, `[ "$1 $2" = "auth token" ] && echo ghs_fromkeyring; exit 0`)

	st, _ := tokenState(t, "github.com/acme/tenants")
	env, err := clusterBootstrapEnv(context.Background(), st)
	if err != nil {
		t.Fatalf("gh holds a token; this must succeed: %v", err)
	}
	if !slices.Contains(env, "GITHUB_TOKEN=ghs_fromkeyring") {
		t.Fatalf("the token gh holds must reach cluster-bootstrap as GITHUB_TOKEN; got %v", env)
	}
}

// No token anywhere must refuse BEFORE the apply, and must not roll back: by phase 5 the
// VPC, the EKS cluster and every substrate component are already built.
func TestClusterBootstrapEnv_RefusesWhenNoTokenIsReachable(t *testing.T) {
	fakeGh(t, `exit 1`)

	st, _ := tokenState(t, "github.com/acme/tenants")
	_, err := clusterBootstrapEnv(context.Background(), st)
	if err == nil {
		t.Fatal("no GitHub credential must refuse — otherwise the apply 401s after the cluster is built")
	}
	var noRollback *engine.NoRollbackError
	if !errors.As(err, &noRollback) {
		t.Fatalf("refusal must NOT trigger the rollback sweep — the cloud is already provisioned; got %T", err)
	}
	if !strings.Contains(err.Error(), "gh auth login") {
		t.Errorf("refusal must name the remedy:\n%s", err)
	}
}

// `gh auth token` can exit 0 and print nothing. Treating a zero exit as success injects
// GITHUB_TOKEN= (empty) and hands the 401 straight back — the green light that means
// nothing this whole preflight/refusal path exists to eliminate.
func TestClusterBootstrapEnv_ZeroExitWithNoTokenIsNotTreatedAsSuccess(t *testing.T) {
	fakeGh(t, `exit 0`) // succeeds, prints nothing

	st, _ := tokenState(t, "github.com/acme/tenants")
	env, err := clusterBootstrapEnv(context.Background(), st)
	if err == nil {
		t.Fatalf("an empty token is not a token — must refuse; got env %v", env)
	}
	for _, e := range env {
		if strings.HasPrefix(e, "GITHUB_TOKEN=") {
			t.Errorf("must never inject an empty GITHUB_TOKEN; got %q", e)
		}
	}
}

// An already-exported token is honoured as-is and NOT copied into the extra env:
// os.Environ() already carries it to the child, and a secret should live in as few
// places as possible.
func TestClusterBootstrapEnv_ExportedTokenIsNotReinjected(t *testing.T) {
	fakeGh(t, `echo SHOULD_NOT_BE_CALLED; exit 0`)
	t.Setenv("GITHUB_TOKEN", "ghp_exported")

	st, _ := tokenState(t, "github.com/acme/tenants")
	env, err := clusterBootstrapEnv(context.Background(), st)
	if err != nil {
		t.Fatalf("an exported token is sufficient: %v", err)
	}
	for _, e := range env {
		if strings.HasPrefix(e, "GITHUB_TOKEN=") {
			t.Errorf("exported token must not be re-injected (os.Environ already carries it); got %q", e)
		}
	}
}

// No tenants repo means the provider is never called, so no credential is required. An
// install without the portal must not be gated on a GitHub token it will never use.
func TestClusterBootstrapEnv_NoTenantsRepoNeedsNoToken(t *testing.T) {
	fakeGh(t, `exit 1`) // no credential anywhere

	st, _ := tokenState(t, "")
	env, err := clusterBootstrapEnv(context.Background(), st)
	if err != nil {
		t.Fatalf("no tenants repo: the github provider is never called, so no token is needed: %v", err)
	}
	if len(env) != 0 {
		t.Fatalf("no tenants repo: inject nothing; got %v", env)
	}
}

// The token must never reach the transcript. exec.Runner echoes argv but never env, which
// is exactly why the credential is passed as env — this test is what stops someone
// "simplifying" it into a -var flag or a note.
func TestClusterBootstrapEnv_TokenNeverAppearsInTheTranscript(t *testing.T) {
	const secret = "ghs_supersecretvalue"
	fakeGh(t, `[ "$1 $2" = "auth token" ] && echo `+secret+`; exit 0`)

	st, out := tokenState(t, "github.com/acme/tenants")
	if _, err := clusterBootstrapEnv(context.Background(), st); err != nil {
		t.Fatalf("clusterBootstrapEnv: %v", err)
	}
	if strings.Contains(out.String(), secret) {
		t.Fatalf("the GitHub token leaked into the operator-facing transcript:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "GITHUB_TOKEN") {
		t.Errorf("the transcript should still SAY a token is being used, just never its value:\n%s", out.String())
	}
}

// network: default vpc_cidr / nat_gateways inject nothing so staging's pin of 3 stands.
func TestNetworkEnv_DefaultSizingInjectsNothing(t *testing.T) {
	cfg := config.Default()
	cfg.Cluster.Name = "platform"
	cfg.ApplyDefaults()
	st := sizingState(cfg)

	env := clusterNetworkEnv(st, "apply")
	for _, e := range env {
		if strings.HasPrefix(e, "TF_VAR_vpc_cidr=") || strings.HasPrefix(e, "TF_VAR_nat_gateways=") {
			// adopt path forces nat_gateways=1 as a neutralizer; create mode must not.
			if !cfg.Cluster.Network.Adopt() {
				t.Errorf("create-mode default sizing must not inject %s; got %v", e, env)
			}
		}
	}

	cfg.Cluster.Network.NATGateways = 3
	env = clusterNetworkEnv(st, "apply")
	if !slices.Contains(env, "TF_VAR_nat_gateways=3") {
		t.Fatalf("non-default nat_gateways must be injected; got %v", env)
	}
}
