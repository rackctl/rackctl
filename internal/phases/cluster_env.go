package phases

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strconv"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/engine"
)

// clusterEnv builds the TF_VARs landing-zone's cluster component declares: the base name,
// the endpoint posture, and the sizing knobs (version + system node group) when they differ
// from config.Default().
//
// The "only when it differs" rule is load-bearing. An ambient TF_VAR beats a leaf's `inputs`
// (F1), so injecting the default version or the default node sizes would silently override
// whatever that environment's leaf carefully chose. Staging and production pin different
// system-node caps than development; wiring Default()'s values unconditionally would
// quietly downgrade both. A dry-run with every field at its default therefore injects only
// cluster_name + the endpoint posture — and a test pins that.
func clusterEnv(ctx context.Context, st *engine.State) ([]string, error) {
	env := []string{"TF_VAR_cluster_name=" + st.Config.Cluster.Name}
	endpointEnv, err := clusterEndpointEnv(ctx, st)
	if err != nil {
		return nil, err
	}
	env = append(env, endpointEnv...)
	env = append(env, clusterSizingEnv(st)...)
	return env, nil
}

// clusterSizingEnv injects cluster_version and system_node_* only when the config differs
// from Default(). Defaults ride no TF_VAR so the leaf wins.
func clusterSizingEnv(st *engine.State) []string {
	d := config.Default().Cluster
	c := st.Config.Cluster
	var env []string

	if c.Version != "" && c.Version != d.Version {
		env = append(env, "TF_VAR_cluster_version="+c.Version)
		note(st, "cluster: TF_VAR_cluster_version=%s — overrides the leaf's default (and any addon "+
			"version pins that assume it). Re-pin eks_addon_versions upstream when you move this",
			c.Version)
	}

	// Compare field-by-field so a partial override (e.g. only maxSize) still injects the
	// full group — landing-zone has no "leave the rest alone" partial, and sending one
	// size without the others would leave the rest at the leaf default while the operator
	// thought they resized the group as a whole.
	nodesDiffer := !slices.Equal(c.SystemNodes.InstanceTypes, d.SystemNodes.InstanceTypes) ||
		c.SystemNodes.MinSize != d.SystemNodes.MinSize ||
		c.SystemNodes.MaxSize != d.SystemNodes.MaxSize ||
		c.SystemNodes.DesiredSize != d.SystemNodes.DesiredSize
	if nodesDiffer {
		types, _ := json.Marshal(c.SystemNodes.InstanceTypes)
		env = append(env,
			"TF_VAR_system_node_instance_types="+string(types),
			"TF_VAR_system_node_min_size="+strconv.Itoa(c.SystemNodes.MinSize),
			"TF_VAR_system_node_max_size="+strconv.Itoa(c.SystemNodes.MaxSize),
			"TF_VAR_system_node_desired_size="+strconv.Itoa(c.SystemNodes.DesiredSize),
		)
		note(st, "cluster: system nodes %s min=%d max=%d desired=%d — leaf defaults overridden",
			string(types), c.SystemNodes.MinSize, c.SystemNodes.MaxSize, c.SystemNodes.DesiredSize)
	}
	return env
}

// agentIAMEnv builds the TF_VARs agent-iam declares that rackctl owns. Today that is just
// bedrock_allowed_model_ids, derived from agentPlatform.bedrockModelFamilies, and only when
// the mapped globs differ from Default()'s mapping — otherwise the leaf's
// ["anthropic.*", "amazon.nova-*"] stands, which is exactly what Default() means.
//
// Without this, narrowing bedrockModelFamilies only narrows the tenant CR's
// identity.allowedModelFamilies (phase 9) while the IAM baseline still grants the full
// fleet default — the config says "only anthropic" and every tenant can still invoke Nova.
func agentIAMEnv(st *engine.State) []string {
	if !st.Config.AgentPlatform.Enabled() {
		return nil
	}
	got := st.Config.AgentPlatform.BedrockModelIDGlobs()
	want := config.Default().AgentPlatform.BedrockModelIDGlobs()
	if len(got) == 0 || slices.Equal(got, want) {
		return nil
	}
	blob, _ := json.Marshal(got)
	note(st, "agent-iam: TF_VAR_bedrock_allowed_model_ids=%s — scopes the tenant baseline Bedrock "+
		"grant to these families (the permissions boundary stays a broad ceiling)", string(blob))
	return []string{"TF_VAR_bedrock_allowed_model_ids=" + string(blob)}
}

// clusterBootstrapEnv builds the TF_VARs cluster-bootstrap declares that are not the
// global booleans already in tgEnv, plus the GitHub credential those TF_VARs make
// mandatory.
//
// tenants_repo_url is the one: when the portal is on, ArgoCD needs a deploy key on the
// tenants repo, and that wiring only fires when this variable is set. Empty (the default)
// disables it, which is correct for a portal-off install and wrong for a portal-on one.
//
// Setting it is also what ARMS cluster-bootstrap's `provider "github"`, whose owner is
// parsed from this very URL. The component says so itself: "the token comes from the
// GITHUB_TOKEN environment variable. When tenants_repo_url is empty, owner is "" and no
// github resources are created, so the provider is never called" (main.tf:170-172). So
// this variable and that credential are one decision, which is why they are built here
// together rather than left to be discovered a phase later as a 401.
func clusterBootstrapEnv(ctx context.Context, st *engine.State) ([]string, error) {
	u := st.Config.Org.GitOps.TenantsGitSSHURL()
	if u == "" {
		return nil, nil
	}
	note(st, "cluster-bootstrap: TF_VAR_tenants_repo_url=%s — registers a read-only deploy key "+
		"and the matching ArgoCD repository credential so portal-committed tenant manifests pull",
		u)
	env := []string{"TF_VAR_tenants_repo_url=" + u}

	tok, source, err := githubToken(ctx, st)
	if err != nil {
		return nil, err
	}
	note(st, "cluster-bootstrap: GITHUB_TOKEN from %s — tenants_repo_url arms the github provider "+
		"and it authenticates with this", source)
	if tok != "" {
		env = append(env, "GITHUB_TOKEN="+tok)
	}
	return env, nil
}

// githubToken resolves the credential cluster-bootstrap's github provider needs, returning
// the token and a description of where it came from.
//
// An exported GITHUB_TOKEN is honoured as-is and returned EMPTY: os.Environ() already
// carries it to the child, so re-injecting a copy would only widen the number of places
// the secret lives.
//
// The gh fallback exists because the documented setup path produces no token at all.
// quickstart tells operators to run `gh auth login`, which stores the credential in gh's
// keyring and exports nothing — so the operator who followed the instructions exactly is
// the operator whose apply 401s. Asking gh for the token it is already holding turns the
// documented path into a working one.
//
// The token is never returned through a note or an error string. exec.Runner echoes argv
// but never env (tools.go:41), so passing it as env is precisely what keeps it out of the
// transcript — do not "improve" this by moving it to a -var flag.
func githubToken(ctx context.Context, st *engine.State) (token, source string, err error) {
	if os.Getenv("GITHUB_TOKEN") != "" {
		return "", "the exported GITHUB_TOKEN", nil
	}
	if st.Runner.DryRun {
		// Capture is a no-op in dry-run, so gh cannot be consulted. Say that plainly rather
		// than reporting a token that was never resolved — preflight is what fails early.
		return "", "gh at apply time (not resolved in a dry-run)", nil
	}
	// Require a non-empty token, not merely a zero exit — see CheckGitHubToken.
	tok, err := st.Runner.Capture(ctx, "gh", "auth", "token")
	if err != nil || tok == "" {
		return "", "", &engine.NoRollbackError{Err: fmt.Errorf(
			"cluster-bootstrap needs a GitHub token and none is reachable.\n\n" +
				"org.gitops.tenantsRepo is set, which sends TF_VAR_tenants_repo_url and arms " +
				"cluster-bootstrap's `provider \"github\"`. That provider authenticates from " +
				"GITHUB_TOKEN and nothing else, so the apply would fail with a 401 — with the VPC, " +
				"the EKS cluster and every substrate component already built.\n\n" +
				"Fix either way:\n" +
				"  gh auth login                          # rackctl bridges the token for you\n" +
				"  export GITHUB_TOKEN=$(gh auth token)   # if you are logged in already\n\n" +
				"The token needs repo scope to register the read-only deploy key. Or unset " +
				"org.gitops.tenantsRepo to skip the tenant-repo wiring entirely")}
	}
	return tok, "`gh auth token`", nil
}
