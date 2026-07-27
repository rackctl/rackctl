package phases

import (
	"context"
	"encoding/json"
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
// global booleans already in tgEnv. tenants_repo_url is the one: when the portal is on,
// ArgoCD needs a deploy key on the tenants repo, and that wiring only fires when this
// variable is set. Empty (the default) disables it, which is correct for a portal-off
// install and wrong for a portal-on one.
func clusterBootstrapEnv(st *engine.State) []string {
	u := st.Config.Org.GitOps.TenantsGitSSHURL()
	if u == "" {
		return nil
	}
	note(st, "cluster-bootstrap: TF_VAR_tenants_repo_url=%s — registers a read-only deploy key "+
		"and the matching ArgoCD repository credential so portal-committed tenant manifests pull",
		u)
	return []string{"TF_VAR_tenants_repo_url=" + u}
}
