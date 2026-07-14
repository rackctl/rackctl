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
	cfg.Addons.Observability = true

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
	cfg.Addons.Observability = false

	if !slices.Contains(tgEnv(cfg), "TF_VAR_enable_managed_monitoring=false") {
		t.Fatalf("observability is off — cluster-bootstrap must be told so, or its SSM read of "+
			"a parameter no component published fails the plan.\ngot: %v", tgEnv(cfg))
	}
}

// The flag and the component must agree, always.
//
// enable_managed_monitoring exists to say "managed-monitoring has already applied and
// published its SSM parameters". CoreComponents is what decides whether that is true. If
// the two ever disagree, cluster-bootstrap either reads a parameter that does not exist
// (plan fails) or skips an annotation that should be stamped (dashboards go Degraded, and
// nothing says why). Bind them here so they cannot drift.
func TestTGEnv_MonitoringFlagAgreesWithCoreComponents(t *testing.T) {
	for _, observability := range []bool{true, false} {
		cfg := &config.Config{}
		cfg.Addons.Observability = observability

		componentRuns := slices.Contains(phases.CoreComponents(cfg), "managed-monitoring")
		flagSet := slices.Contains(tgEnv(cfg), "TF_VAR_enable_managed_monitoring=true")

		if componentRuns != flagSet {
			t.Errorf("observability=%v: CoreComponents runs managed-monitoring=%v but "+
				"TF_VAR_enable_managed_monitoring=%v — the flag means 'that component has run "+
				"and published its SSM parameters', so these can never disagree",
				observability, componentRuns, flagSet)
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
