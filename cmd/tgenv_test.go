package cmd

import (
	"slices"
	"testing"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/phases"
)

// The catalog only ever learns per-cluster values from the annotations cluster-bootstrap
// stamps on the ArgoCD cluster Secret. cluster-bootstrap stamps the monitoring ones
// behind `enable_managed_monitoring`, which defaults to FALSE — and rackctl never passed
// it.
//
// So rackctl ran managed-monitoring, created the AMP and AMG workspaces, published all
// three SSM parameters, and then left cluster-bootstrap believing monitoring was off. The
// annotations were never stamped, the dashboards ApplicationSet had nothing to inject,
// and the Grafana CR was rejected by its own CRD:
//
//	spec.external.url: Invalid value: "": in body should match '^https?://.+$'
//
// Every other Application was Healthy. The AMG workspace was up. The token was valid.
func TestTGEnv_PassesManagedMonitoringWhenObservabilityIsOn(t *testing.T) {
	cfg := &config.Config{}
	cfg.Observability.Tier = config.TierFull

	if !slices.Contains(tgEnv(cfg), "TF_VAR_enable_managed_monitoring=true") {
		t.Fatalf("observability is on, but cluster-bootstrap is not told — it will not stamp "+
			"monitoring/grafana-url and the dashboards Grafana CR renders with an empty url.\ngot: %v",
			tgEnv(cfg))
	}
}

// And it must be FALSE when observability is off — a cluster that never ran
// managed-monitoring has no SSM parameters to read, and cluster-bootstrap's plan fails on
// the missing parameter rather than merely skipping the annotation. That is why the
// variable is opt-in in the first place, and why passing a blanket `true` would be a
// different bug rather than a fix.
func TestTGEnv_DoesNotClaimMonitoringWhenObservabilityIsOff(t *testing.T) {
	cfg := &config.Config{}
	cfg.Observability.Tier = config.TierFloor

	if !slices.Contains(tgEnv(cfg), "TF_VAR_enable_managed_monitoring=false") {
		t.Fatalf("observability is off — cluster-bootstrap must be told so, or its SSM read of "+
			"a parameter no component published fails the plan.\ngot: %v", tgEnv(cfg))
	}
}

// One tier field drives three things — whether managed-monitoring is applied, the tier
// label, and the Grafana flag — and this asserts all three stay consistent for every tier.
//
// enable_managed_monitoring means "managed-monitoring has already applied and published its
// SSM parameters", and CoreComponents is what decides whether that is true. If the two ever
// disagree, cluster-bootstrap either reads a parameter that does not exist (plan fails) or
// skips an annotation that should be stamped (dashboards go Degraded, and nothing says why).
//
// The label half is the one that bites hardest. cluster-bootstrap takes observability_tier
// and enable_managed_monitoring as INDEPENDENT variables with nothing relating them, and
// every committed leaf pins tier=full. So the old `addons.observability: false` set the flag
// false, left the leaf's tier=full standing, and produced a cluster LABELLED full with no
// AMP behind it — the tier-gated secret-stores ExternalSecret then looked for an AMP
// endpoint that was never published and sat in permanent SecretSyncedError. Deriving both
// from one field is what makes that unreachable.
func TestTGEnv_TierDrivesTheComponentAndBothFlags(t *testing.T) {
	for _, tc := range []struct {
		tier       config.ObservabilityTier
		wantMonito bool
	}{
		{config.TierFull, true},
		{config.TierFloor, false},
	} {
		cfg := &config.Config{}
		cfg.Observability.Tier = tc.tier
		env := tgEnv(cfg)

		componentRuns := slices.Contains(phases.CoreComponents(cfg), "managed-monitoring")
		flagSet := slices.Contains(env, "TF_VAR_enable_managed_monitoring=true")

		if componentRuns != tc.wantMonito {
			t.Errorf("tier=%s: CoreComponents applies managed-monitoring=%v, want %v",
				tc.tier, componentRuns, tc.wantMonito)
		}
		if componentRuns != flagSet {
			t.Errorf("tier=%s: CoreComponents applies managed-monitoring=%v but "+
				"TF_VAR_enable_managed_monitoring=%v — the flag means 'that component has run "+
				"and published its SSM parameters', so these can never disagree",
				tc.tier, componentRuns, flagSet)
		}
		// The label must always be sent, and must always match the tier. A cluster whose
		// label disagrees with its substrate is the SecretSyncedError case above; a cluster
		// with a blank label matches no generator at all and gets the node agent only.
		if want := "TF_VAR_observability_tier=" + string(tc.tier); !slices.Contains(env, want) {
			t.Errorf("tier=%s: %q not injected — the tier-aware eks-gitops ApplicationSets select "+
				"on this label, and several derive Helm parameters from it; got %v", tc.tier, want, env)
		}
	}
}

// The catalog fork URL must reach terragrunt too. Without it, cluster-bootstrap's
// gitops_repo_url fell back to the UPSTREAM catalog and every install synced app-of-apps
// from someone else's main branch while the org's own fork sat unread.
func TestTGEnv_PassesTheOrgsForkNotUpstream(t *testing.T) {
	cfg := &config.Config{}
	cfg.Org.Name = "acme"
	cfg.Org.GitOps.EKSGitopsRepo = "github.com/acme/eks-gitops"

	env := tgEnv(cfg)
	if !slices.Contains(env, "TF_VAR_gitops_repo_url=https://github.com/acme/eks-gitops.git") {
		t.Fatalf("the org's fork must be passed to terragrunt, or app-of-apps syncs from "+
			"upstream and the fork is inert.\ngot: %v", env)
	}
}

// addons.accelerators must reach the label that makes it mean something. It was documented
// in the shipped example config and read by nothing, so setting it produced a cluster with
// no accelerator label and no GPU addons — the config said yes and the platform said nothing.
func TestTGEnv_PassesTheAcceleratorLabel(t *testing.T) {
	for _, on := range []bool{true, false} {
		cfg := &config.Config{}
		cfg.Addons.Accelerators = on
		want := "TF_VAR_enable_accelerators=" + map[bool]string{true: "true", false: "false"}[on]
		if !slices.Contains(tgEnv(cfg), want) {
			t.Errorf("accelerators=%v: %q not injected — without it the accelerators ApplicationSet "+
				"never targets the cluster and the config knob is inert", on, want)
		}
	}
}

// enable_agent_platform must track agentPlatform.enable. Without it, agentPlatform.enable:
// false still leaves cluster-bootstrap's default true, so ArgoCD installs an operator whose
// IAM role was never created (agent-iam is also skipped). The flag and the component gate
// have to agree.
func TestTGEnv_AgentPlatformFlagTracksTheConfig(t *testing.T) {
	for _, tc := range []struct {
		enable *bool
		want   string
	}{
		{nil, "TF_VAR_enable_agent_platform=true"},                         // omitted = on
		{boolPtr(true), "TF_VAR_enable_agent_platform=true"},
		{boolPtr(false), "TF_VAR_enable_agent_platform=false"},
	} {
		cfg := &config.Config{}
		cfg.AgentPlatform.Enable = tc.enable
		if !slices.Contains(tgEnv(cfg), tc.want) {
			t.Errorf("enable=%v: want %q in tgEnv, got %v", tc.enable, tc.want, tgEnv(cfg))
		}
	}
}

// enable_external_dns must track whether the dns component is applied. Every committed
// workload leaf pins it true and the development leaf depends on dns — a config with no
// dns: block fails cluster-bootstrap on a missing SSM parameter without this override.
func TestTGEnv_ExternalDNSTracksTheDNSBlock(t *testing.T) {
	off := &config.Config{}
	if !slices.Contains(tgEnv(off), "TF_VAR_enable_external_dns=false") {
		t.Fatalf("no dns block: enable_external_dns must be false, or the leaf's true fails the "+
			"SSM read. got %v", tgEnv(off))
	}

	on := &config.Config{DNS: &config.DNS{HostedZone: "acme.example.com"}}
	if !slices.Contains(tgEnv(on), "TF_VAR_enable_external_dns=true") {
		t.Fatalf("dns block set: enable_external_dns must be true. got %v", tgEnv(on))
	}
}

// enable_portal_reader follows controlPlane.portal so a portal-enabled install actually
// gets the reader SA. Leaving it at the leaf default (false) made the portal knob inert.
func TestTGEnv_PortalReaderTracksThePortalFlag(t *testing.T) {
	off := &config.Config{}
	if !slices.Contains(tgEnv(off), "TF_VAR_enable_portal_reader=false") {
		t.Fatalf("portal off: enable_portal_reader must be false. got %v", tgEnv(off))
	}
	on := &config.Config{}
	on.ControlPlane.Portal = true
	if !slices.Contains(tgEnv(on), "TF_VAR_enable_portal_reader=true") {
		t.Fatalf("portal on: enable_portal_reader must be true. got %v", tgEnv(on))
	}
}

func boolPtr(b bool) *bool { return &b }
