package phases

import (
	"context"
	"io"
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
	if env := clusterBootstrapEnv(st); len(env) != 0 {
		t.Fatalf("no tenants repo: inject nothing; got %v", env)
	}

	st.Config.Org.GitOps.TenantsRepo = "github.com/acme/tenants"
	env := clusterBootstrapEnv(st)
	if !slices.Contains(env, "TF_VAR_tenants_repo_url=git@github.com:acme/tenants.git") {
		t.Fatalf("tenants repo must become an SSH URL for cluster-bootstrap; got %v", env)
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
