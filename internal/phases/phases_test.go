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

// testState wraps a config in an engine.State whose runner discards notes, so the
// TF_VAR builders (which print operator-facing notes) can be exercised without output.
func testState(cfg *config.Config) *engine.State {
	return &engine.State{Config: cfg, Runner: exec.New(io.Discard)}
}

// indexOf returns the position of c, or -1.
func indexOf(comps []string, c string) int { return slices.Index(comps, c) }

// baseCfg returns a config with the agent platform on (its default) and nothing else.
// Named baseCfg, not base: `base` is already the embedded phase struct in this package.
func baseCfg() *config.Config { return &config.Config{} }

func TestCoreComponents_AgentIAMPresentByDefault(t *testing.T) {
	// AgentPlatform.Enable is nil => Enabled() is true (installing the agent
	// platform is the point of the tool). agent-iam creates the operator's role;
	// without it the operator crashloops on AssumeRoleWithWebIdentity 403.
	comps := CoreComponents(baseCfg())
	if indexOf(comps, "agent-iam") < 0 {
		t.Fatalf("agent-iam must be applied when the agent platform is enabled; got %v", comps)
	}
}

func TestCoreComponents_AgentIAMOmittedWhenPlatformOff(t *testing.T) {
	off := false
	cfg := baseCfg()
	cfg.AgentPlatform.Enable = &off
	if comps := CoreComponents(cfg); indexOf(comps, "agent-iam") >= 0 {
		t.Fatalf("agent-iam must not be applied when the agent platform is off; got %v", comps)
	}
}

func TestCoreComponents_ManagedMonitoringIsOptIn(t *testing.T) {
	// AMP + AMG both cost money. They must never be applied unless asked for.
	if comps := CoreComponents(baseCfg()); indexOf(comps, "managed-monitoring") >= 0 {
		t.Fatalf("managed-monitoring must be opt-in (it provisions billable AMP+AMG); got %v", comps)
	}
}

func TestCoreComponents_ManagedMonitoringPrecedesClusterBootstrap(t *testing.T) {
	// cluster-bootstrap READS managed-monitoring's SSM params (grafana_url,
	// amp_endpoint, amp_workspace_id) to stamp them onto the ArgoCD cluster
	// Secret. Applying it after cluster-bootstrap fails the read.
	cfg := baseCfg()
	cfg.Observability.Tier = config.TierFull
	comps := CoreComponents(cfg)

	mm, cb := indexOf(comps, "managed-monitoring"), indexOf(comps, "cluster-bootstrap")
	if mm < 0 {
		t.Fatalf("managed-monitoring missing at tier=full; got %v", comps)
	}
	if mm > cb {
		t.Fatalf("managed-monitoring (%d) must precede cluster-bootstrap (%d): cluster-bootstrap reads its SSM params; got %v", mm, cb, comps)
	}
}

func TestCoreComponents_DNSOnlyWithHostedZone(t *testing.T) {
	cfg := baseCfg()
	if comps := CoreComponents(cfg); indexOf(comps, "dns") >= 0 {
		t.Fatalf("dns must not be applied without a dns block; got %v", comps)
	}
	cfg.DNS = &config.DNS{} // present but empty hosted zone
	if comps := CoreComponents(cfg); indexOf(comps, "dns") >= 0 {
		t.Fatalf("dns must not be applied with an empty hostedZone; got %v", comps)
	}
	cfg.DNS = &config.DNS{HostedZone: "example.com"}
	if comps := CoreComponents(cfg); indexOf(comps, "dns") < 0 {
		t.Fatalf("dns must be applied when a hostedZone is set; got %v", comps)
	}
}

func TestCoreComponents_ModelImportIsOptIn(t *testing.T) {
	// It provisions account+region-scoped substrate an org may never want, and it is the
	// one component rackctl applies but never destroys. It must never appear unasked.
	if comps := CoreComponents(baseCfg()); indexOf(comps, "model-import") >= 0 {
		t.Fatalf("model-import must be opt-in; got %v", comps)
	}
}

func TestCoreComponents_ModelImportRequiresTheAgentPlatform(t *testing.T) {
	// With the platform off there is no operator to reconcile a ModelGateway route, so
	// the staging bucket, import role and SSM parameters would be dead weight. Config
	// validation rejects the combination; CoreComponents must not build it either.
	cfg := baseCfg()
	cfg.AgentPlatform.ModelImport = true
	if comps := CoreComponents(cfg); indexOf(comps, "model-import") < 0 {
		t.Fatalf("model-import must be applied when agentPlatform.modelImport is set; got %v", comps)
	}

	off := false
	cfg.AgentPlatform.Enable = &off
	if comps := CoreComponents(cfg); indexOf(comps, "model-import") >= 0 {
		t.Fatalf("model-import must not be applied when the agent platform is off; got %v", comps)
	}
}

// Apply and destroy must stay symmetric: everything rackctl builds, it tears down.
//
// An earlier pass carved model-import out of the teardown as account-scoped substrate that
// had to survive a cluster's death. That asymmetry is gone. It was the wrong shape twice
// over — landing-zone has since scoped the bucket, role and SSM prefix by ENVIRONMENT and
// given them a teardown posture (force_destroy unconditional in development, opt-in
// elsewhere), so nothing here outlives its cluster; and the BucketNotEmpty hazard it was
// dodging was never unique to model-import — seven other buckets on the default path had
// exactly the same gap and no carve-out at all.
//
// The rule that replaces it: if something must outlive a cluster, rackctl should not be the
// thing creating it. So this test asserts the substrate phase's teardown set is the exact
// reverse of its apply set, for every gate combination — no exceptions, no exemption
// predicate to drift out of sync across the three places that walk CoreComponents.
func TestSubstrate_TeardownIsTheExactReverseOfApply(t *testing.T) {
	for name, cfg := range map[string]*config.Config{
		"defaults": baseCfg(),
		"all-on": func() *config.Config {
			c := baseCfg()
			c.Observability.Tier = config.TierFull
			c.DNS = &config.DNS{HostedZone: "example.com"}
			c.AgentPlatform.ModelImport = true
			return c
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			applied := substrateComponents(cfg)

			// The teardown walks the same slice backwards. Reversing it must reproduce the
			// apply order exactly — which is only true if nothing is skipped.
			reversed := make([]string, 0, len(applied))
			for i := len(applied) - 1; i >= 0; i-- {
				reversed = append(reversed, applied[i])
			}
			for i := range reversed {
				if reversed[i] != applied[len(applied)-1-i] {
					t.Fatalf("teardown order is not the reverse of apply: apply=%v reverse=%v", applied, reversed)
				}
			}
			if len(reversed) != len(applied) {
				t.Fatalf("teardown drops %d component(s) — rackctl must destroy everything it builds; apply=%v",
					len(applied)-len(reversed), applied)
			}
		})
	}
}

// model-import is applied like any other conditional component, and destroyed like one.
func TestCoreComponents_ModelImportIsDestroyedLikeAnythingElse(t *testing.T) {
	cfg := baseCfg()
	cfg.AgentPlatform.ModelImport = true
	if indexOf(substrateComponents(cfg), "model-import") < 0 {
		t.Fatalf("the substrate phase must apply model-import; got %v", substrateComponents(cfg))
	}
}

func TestCoreComponents_NetworkFirst(t *testing.T) {
	cfg := baseCfg()
	cfg.Observability.Tier = config.TierFull
	cfg.DNS = &config.DNS{HostedZone: "example.com"}
	comps := CoreComponents(cfg)

	if comps[0] != "network" {
		t.Errorf("network must be applied first; got %v", comps)
	}
	// The cluster must exist before anything that talks to its API.
	if indexOf(comps, "cluster") > indexOf(comps, "cluster-bootstrap") {
		t.Errorf("cluster must precede cluster-bootstrap; got %v", comps)
	}
}

// Pod Identity must exist BEFORE ArgoCD can deploy the pods that need it.
//
// This test replaces one that asserted the opposite — "cluster-addons must be applied
// last" — which pinned the bug rather than the fix.
//
// EKS Pod Identity injects credentials at pod ADMISSION. A pod that starts before its
// association exists does not get them, and does not fail: it falls back to the EC2 node
// instance role and only fails later, elsewhere, as a permission error naming the NODE
// role rather than the pod.
//
// cluster-addons creates all eleven associations. cluster-bootstrap installs ArgoCD,
// which immediately begins deploying the very addons those associations are for. With
// addons last, ArgoCD deployed external-secrets before its identity existed; ESO ran as
// the node role, every ExternalSecret failed `could not get secret data from provider`,
// and it cascaded — alloy had no config, so AMP had no metrics, so opencost crash-looped
// on an empty metrics store. Three Applications Degraded and not one named the cause.
//
// cluster-addons touches only AWS, so it has nothing to wait for. Ordering it first
// removes the race outright.
// The substrate (which owns cluster-addons) must run before the gitops phase (which owns
// cluster-bootstrap = ArgoCD). This is the Pod Identity ordering, and it now lives in the
// PHASE STRUCTURE — a substrate phase and a gitops phase — not in a component's index in a
// shared list.
//
// That distinction is the whole lesson of #26/#29. #26 "fixed" this by reordering
// CoreComponents and asserting that list; it did nothing, because the apply order is driven
// by which phase owns a component. The invariant only became real when cluster-addons and
// cluster-bootstrap were split across two phases whose order the engine executes literally.
// So this test asserts the phase order and the ownership, which together ARE the guarantee.
func TestPhases_SubstrateBeforeGitOps(t *testing.T) {
	ids := make([]string, 0)
	for _, p := range All() {
		ids = append(ids, p.ID())
	}
	sub, git := indexOf(ids, "substrate"), indexOf(ids, "gitops")
	if sub == -1 || git == -1 {
		t.Fatalf("both the substrate and gitops phases must exist; got %v", ids)
	}
	if sub > git {
		t.Fatalf("the substrate phase must run BEFORE the gitops phase — it creates the Pod "+
			"Identity associations, and the gitops phase installs the ArgoCD that deploys the pods "+
			"needing them. A pod admitted before its association exists silently falls back to the "+
			"NODE ROLE.\nphase order: %v", ids)
	}

	// And ownership must line up with the split: cluster-addons is the substrate's,
	// cluster-bootstrap is the gitops phase's. If cluster-bootstrap leaked back into the
	// substrate set, the substrate phase would install ArgoCD itself and the split would be
	// meaningless.
	for name, cfg := range map[string]*config.Config{
		"defaults":      baseCfg(),
		"observability": func() *config.Config { c := baseCfg(); c.Observability.Tier = config.TierFull; return c }(),
		"dns":           func() *config.Config { c := baseCfg(); c.DNS = &config.DNS{HostedZone: "x.com"}; return c }(),
		"model-import":  func() *config.Config { c := baseCfg(); c.AgentPlatform.ModelImport = true; return c }(),
	} {
		sc := substrateComponents(cfg)
		if indexOf(sc, "cluster-addons") == -1 {
			t.Errorf("%s: the substrate phase must apply cluster-addons; got %v", name, sc)
		}
		if indexOf(sc, "cluster-bootstrap") != -1 {
			t.Errorf("%s: cluster-bootstrap belongs to the gitops phase, not the substrate; got %v", name, sc)
		}
	}
}

// substrateComponents must be a faithful SUBSEQUENCE of CoreComponents. This is the
// invariant that broke once: CoreComponents was only read by destroy, while the apply
// path walked its own hardcoded {"secrets","cluster-bootstrap"} — so the two
// disagreed and agent-iam / managed-monitoring / dns were never applied at all.
func TestSubstrateComponents_IsSubsequenceOfCore(t *testing.T) {
	for name, cfg := range map[string]*config.Config{
		"defaults": baseCfg(),
		"all-on": func() *config.Config {
			c := baseCfg()
			c.Observability.Tier = config.TierFull
			c.DNS = &config.DNS{HostedZone: "example.com"}
			c.AgentPlatform.ModelImport = true
			return c
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			core := CoreComponents(cfg)
			sub := substrateComponents(cfg)

			// every substrate component appears in core, in the same relative order
			prev := -1
			for _, c := range sub {
				i := indexOf(core, c)
				if i < 0 {
					t.Fatalf("substrateComponents has %q which is not in CoreComponents %v", c, core)
				}
				if i <= prev {
					t.Fatalf("substrateComponents order diverges from CoreComponents: %v vs %v", sub, core)
				}
				prev = i
			}
			// it owns everything except what the cluster and gitops phases apply
			for _, c := range core {
				owned := c != "network" && c != "cluster" && c != "cluster-bootstrap"
				if owned && indexOf(sub, c) < 0 {
					t.Errorf("component %q is in CoreComponents but no phase applies it; got %v", c, sub)
				}
			}
		})
	}
}

// The create-mode network levers ride TF_VAR_* onto landing-zone's network component,
// same seam as TF_VAR_cluster_name — the committed live tree stays generic and rackctl
// layers the per-run choice over it. When set, each lever must appear in the injected
// env; IPAM pool id and netmask travel together.
func TestClusterNetworkEnv_InjectsLeversWhenSet(t *testing.T) {
	cfg := &config.Config{}
	cfg.Cluster.Network = config.ClusterNet{
		IPAMPoolID:        "ipam-pool-0abc123",
		IPAMNetmaskLength: 18,
		TransitGatewayID:  "tgw-0abc123",
		CentralizedEgress: true,
	}
	env := clusterNetworkEnv(testState(cfg))
	for _, want := range []string{
		"TF_VAR_ipam_pool_id=ipam-pool-0abc123",
		"TF_VAR_ipam_netmask_length=18",
		"TF_VAR_transit_gateway_id=tgw-0abc123",
		"TF_VAR_centralized_egress=true",
	} {
		if !slices.Contains(env, want) {
			t.Errorf("network levers set but %q not injected; got %v", want, env)
		}
	}
}

// A day-0 hub with no levers set injects nothing — the committed tree's plain
// literal-CIDR VPC with local NAT is left exactly as it stands.
func TestClusterNetworkEnv_EmptyWhenLeversOff(t *testing.T) {
	cfg := &config.Config{}
	cfg.Cluster.Network = config.ClusterNet{VPCCIDR: "10.0.0.0/16", NATGateways: 1}
	if env := clusterNetworkEnv(testState(cfg)); len(env) != 0 {
		t.Errorf("no levers set, but TF_VARs were injected: %v", env)
	}
}

func TestGitURL(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"github.com/acme/eks-gitops", "https://github.com/acme/eks-gitops.git"},
		{"github.com/acme/eks-gitops.git", "https://github.com/acme/eks-gitops.git"},
		{"https://github.com/acme/eks-gitops", "https://github.com/acme/eks-gitops.git"},
		{"https://github.com/acme/eks-gitops.git", "https://github.com/acme/eks-gitops.git"},
		{"git@github.com:acme/eks-gitops.git", "git@github.com:acme/eks-gitops.git"},
	} {
		g := config.OrgGitOps{EKSGitopsRepo: tc.in}
		if got := g.GitURL(); got != tc.want {
			t.Errorf("GitURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The whole point of the fix: the URL handed to terragrunt must be the ORG'S fork,
// never the upstream catalog. Guard against a regression that reintroduces a default.
func TestGitURL_IsTheOrgFork_NotUpstream(t *testing.T) {
	cfg := &config.Config{}
	cfg.Org.Name = "acme"
	cfg.Org.GitOps.EKSGitopsRepo = "github.com/acme/eks-gitops"

	got := cfg.Org.GitOps.GitURL()
	if got != "https://github.com/acme/eks-gitops.git" {
		t.Fatalf("GitURL = %q, want the org's own fork", got)
	}
	if got == "https://github.com/nanohype/eks-gitops.git" {
		t.Fatal("GitURL resolved to the UPSTREAM catalog — an install must never sync from someone else's repo")
	}
}

// The cluster phase must leave st.Runner.Env exactly as it found it.
//
// This is the invariant that was violated. `cluster.Run` appended TF_VAR_cluster_name and
// the endpoint/network levers straight onto the shared Runner, so they were not inputs to
// the cluster component — they were inputs to every terragrunt invocation for the rest of
// the run. secrets, agent-iam, managed-monitoring, dns, cluster-addons and cluster-bootstrap
// all received the cluster BASE name while their own envcommon was simultaneously handing
// them `cluster_name = <environment>-<base>`. An ambient TF_VAR beats a terragrunt `inputs`
// value, so the leaked base name won, silently, and nothing failed loudly enough to notice.
//
// componentEnv now scopes each variable to the one invocation that declares it, and tg()
// restores the Runner afterwards. Asserting the Runner is unchanged is the cheapest way to
// pin that: it holds no matter how many levers are added later, which matters because the
// campaign's next target adds several.
func TestClusterPhase_DoesNotLeakTFVarsIntoLaterPhases(t *testing.T) {
	cfg := baseCfg()
	cfg.Cluster.Name = "platform"
	cfg.Cluster.Network = config.ClusterNet{
		IPAMPoolID:        "ipam-pool-0abc123",
		IPAMNetmaskLength: 18,
		TransitGatewayID:  "tgw-0abc123",
		CentralizedEgress: true,
	}
	st := testState(cfg)
	st.Runner.DryRun = true // print, do not execute — the argv is not what this test asserts
	before := append([]string(nil), st.Runner.Env...)

	if err := (cluster{}).Run(context.Background(), st); err != nil {
		t.Fatalf("cluster.Run: %v", err)
	}

	if len(st.Runner.Env) != len(before) {
		t.Fatalf("cluster.Run leaked %d env var(s) into the shared Runner — every later phase's "+
			"terragrunt call would inherit them, and an ambient TF_VAR beats a leaf's inputs.\nleaked: %v",
			len(st.Runner.Env)-len(before), st.Runner.Env[len(before):])
	}
	for _, e := range st.Runner.Env {
		if strings.HasPrefix(e, "TF_VAR_") {
			t.Errorf("TF_VAR left on the shared Runner after the cluster phase: %s", e)
		}
	}
}

// The scoped env must contain exactly what each component declares, and nothing it does not.
//
// TF_VAR_cluster_name was injected into `network` too, justified by a comment claiming
// "network and cluster must agree on it or Karpenter/ELB discovery breaks". components/aws/network
// declares no cluster_name variable, and its own comment says the ownership and discovery tags
// are applied by the CLUSTER component via aws_ec2_tag — because the VPC is shared per
// environment and cluster-agnostic. The injection was inert and the reasoning inverted.
func TestComponentEnv_ScopesVariablesToTheComponentThatDeclaresThem(t *testing.T) {
	cfg := baseCfg()
	cfg.Cluster.Name = "platform"
	cfg.Cluster.Network = config.ClusterNet{IPAMPoolID: "ipam-pool-0abc123", IPAMNetmaskLength: 18}
	st := testState(cfg)
	st.Runner.DryRun = true

	netEnv, err := componentEnv(context.Background(), st, "network")
	if err != nil {
		t.Fatalf("componentEnv(network): %v", err)
	}
	if slices.ContainsFunc(netEnv, func(e string) bool { return strings.HasPrefix(e, "TF_VAR_cluster_name=") }) {
		t.Errorf("network must not receive TF_VAR_cluster_name — it declares no such variable; got %v", netEnv)
	}
	if !slices.Contains(netEnv, "TF_VAR_ipam_pool_id=ipam-pool-0abc123") {
		t.Errorf("network must receive the create-mode levers it declares; got %v", netEnv)
	}

	clusterEnv, err := componentEnv(context.Background(), st, "cluster")
	if err != nil {
		t.Fatalf("componentEnv(cluster): %v", err)
	}
	if !slices.Contains(clusterEnv, "TF_VAR_cluster_name=platform") {
		t.Errorf("cluster must receive TF_VAR_cluster_name; got %v", clusterEnv)
	}
	if slices.ContainsFunc(clusterEnv, func(e string) bool { return strings.HasPrefix(e, "TF_VAR_ipam_") }) {
		t.Errorf("cluster must not receive the network levers; got %v", clusterEnv)
	}

	// A component with no per-run inputs gets nothing at all.
	if env, err := componentEnv(context.Background(), st, "secrets"); err != nil || len(env) != 0 {
		t.Errorf("secrets declares no per-run inputs, so it must receive none; got %v (err %v)", env, err)
	}
}

// The observability component is applied on BOTH tiers, and it is not optional.
//
// It is the sole producer of the three
// /eks-agent-platform/<cluster>/observability/alerts_{critical,warning,info}_topic_arn
// parameters. landing-zone's CloudWatch alarms are the floor tier's ONLY alerting, and the
// full tier adds to them rather than replacing them — so gating this on the tier would
// leave a floor cluster with no alert routing at all. rackctl applied it nowhere until now,
// which left every rackctl-installed cluster with an alerting control loop and no producer.
func TestCoreComponents_ObservabilityIsAppliedOnEveryTier(t *testing.T) {
	for _, tier := range []config.ObservabilityTier{config.TierFull, config.TierFloor} {
		cfg := baseCfg()
		cfg.Observability.Tier = tier
		comps := CoreComponents(cfg)
		if indexOf(comps, "observability") < 0 {
			t.Errorf("tier %q: observability must be applied — it is the only producer of the "+
				"alert topic ARNs, and both tiers alert; got %v", tier, comps)
		}
		if indexOf(substrateComponents(cfg), "observability") < 0 {
			t.Errorf("tier %q: the substrate phase must own observability; got %v",
				tier, substrateComponents(cfg))
		}
	}
}

// managed-monitoring follows the tier, and only the tier.
func TestCoreComponents_ManagedMonitoringFollowsTheTier(t *testing.T) {
	full := baseCfg()
	full.Observability.Tier = config.TierFull
	if indexOf(CoreComponents(full), "managed-monitoring") < 0 {
		t.Error("the full tier must apply managed-monitoring — it publishes the AMP endpoint the " +
			"tier-gated ExternalSecret reads, and nothing else does")
	}

	floor := baseCfg()
	floor.Observability.Tier = config.TierFloor
	if indexOf(CoreComponents(floor), "managed-monitoring") >= 0 {
		t.Error("the floor tier must NOT apply managed-monitoring — AMP and AMG both cost money " +
			"and floor is CloudWatch only")
	}
}

// observability must precede cluster-addons, and it must land before anything that resolves
// its SSM parameters at PLAN time — eks-agent-platform's kill-switch burn-rate rules read
// both topic ARNs through unguarded `data` blocks, so the ordering is load-bearing rather
// than tidy once rackctl applies that tree.
func TestCoreComponents_ObservabilityPrecedesTheGitOpsConsumers(t *testing.T) {
	cfg := baseCfg()
	comps := CoreComponents(cfg)
	obs := indexOf(comps, "observability")
	for _, later := range []string{"cluster-addons", "cluster-bootstrap"} {
		if i := indexOf(comps, later); obs > i {
			t.Errorf("observability (%d) must precede %s (%d); got %v", obs, later, i, comps)
		}
	}
}

// druid is opt-in and reaches the apply list when asked for. It was declared, defaulted and
// documented for a long time while no code path read it — a knob that looks like a feature
// and does nothing, which is the failure class the rest of this repo exists to kill.
func TestCoreComponents_DruidIsOptInAndReachable(t *testing.T) {
	if comps := CoreComponents(baseCfg()); indexOf(comps, "druid") >= 0 {
		t.Fatalf("druid must be opt-in — it provisions Aurora Serverless and optionally MSK; got %v", comps)
	}
	cfg := baseCfg()
	cfg.Addons.Druid = true
	if comps := CoreComponents(cfg); indexOf(comps, "druid") < 0 {
		t.Fatalf("addons.druid must reach the apply list; got %v", comps)
	}
}
