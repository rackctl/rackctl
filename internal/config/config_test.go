package config

import "testing"

func valid() *Config {
	c := Default()
	c.Org.Name = "acme"
	c.Cloud.AccountID = "111111111111"
	c.Cloud.Profile = "workload-development"
	c.Cluster.Name = "platform"
	c.ApplyDefaults()
	return c
}

// Default() pins the same cluster_version landing-zone does, so a config that trusts the
// example is not silently lying about which Kubernetes the leaf will install.
func TestDefault_ClusterVersionMatchesLandingZone(t *testing.T) {
	if got := Default().Cluster.Version; got != "1.36" {
		t.Fatalf("Default().Cluster.Version = %q, want 1.36 — landing-zone's cluster_version default", got)
	}
}

// TenantsGitSSHURL is what cluster-bootstrap's tenants_repo_url wants. Bare, https and
// already-SSH forms all land on git@github.com:… so the deploy-key wiring parses them.
func TestTenantsGitSSHURL(t *testing.T) {
	cases := map[string]string{
		"":                                    "",
		"github.com/acme/tenants":             "git@github.com:acme/tenants.git",
		"https://github.com/acme/tenants.git": "git@github.com:acme/tenants.git",
		"git@github.com:acme/tenants.git":     "git@github.com:acme/tenants.git",
	}
	for in, want := range cases {
		got := OrgGitOps{TenantsRepo: in}.TenantsGitSSHURL()
		if got != want {
			t.Errorf("TenantsGitSSHURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// BedrockModelIDGlobs maps the config's family names onto agent-iam's IAM globs. The
// default families must produce landing-zone's own default so a default-valued config
// injects nothing.
func TestBedrockModelIDGlobs(t *testing.T) {
	got := Default().AgentPlatform.BedrockModelIDGlobs()
	want := []string{"anthropic.*", "amazon.nova-*"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("Default globs = %v, want %v — must match agent-iam's default so we inject nothing", got, want)
	}
	if g := (AgentPlatform{BedrockModelFamilies: []string{"anthropic"}}).BedrockModelIDGlobs(); len(g) != 1 || g[0] != "anthropic.*" {
		t.Fatalf("anthropic → anthropic.*, got %v", g)
	}
}

func TestValidate(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatalf("valid config errored: %v", err)
	}

	cases := map[string]func(*Config){
		"missing org.name":                           func(c *Config) { c.Org.Name = "" },
		"short account id":                           func(c *Config) { c.Cloud.AccountID = "123" },
		"non-aws provider":                           func(c *Config) { c.Cloud.Provider = ProviderAzure },
		"bad environment":                            func(c *Config) { c.Environment = "qa" },
		"missing cluster.name":                       func(c *Config) { c.Cluster.Name = "" },
		"bad cluster.name":                           func(c *Config) { c.Cluster.Name = "Platform_1" },
		"cluster.name too long":                      func(c *Config) { c.Cluster.Name = "thirteenchars" }, // 13 > 12 char cap
		"cluster.name == env":                        func(c *Config) { c.Cluster.Name = string(c.Environment) },
		"prod public endpoint":                       func(c *Config) { c.Environment = EnvProduction; c.Cluster.EndpointPublicAccess = true },
		"bad cidr in allowlist":                      func(c *Config) { c.Cluster.EndpointAllowlist = []string{"10.0.0.0"} }, // no mask
		"bare ip in allowlist":                       func(c *Config) { c.Cluster.EndpointAllowlist = []string{"203.0.113.4"} },
		"eksFleet no clustersRepo":                   func(c *Config) { c.ControlPlane.EKSFleet = true },
		"portal no tenantsRepo":                      func(c *Config) { c.ControlPlane.Portal = true },
		"centralized egress without transit gateway": func(c *Config) { c.Cluster.Network.CentralizedEgress = true },
		"ipam pool with a non-default vpc cidr": func(c *Config) {
			c.Cluster.Network.IPAMPoolID = "ipam-pool-0abc123"
			c.Cluster.Network.IPAMNetmaskLength = 16
			c.Cluster.Network.VPCCIDR = "10.20.0.0/16"
		},
		"modelImport in a region without Custom Model Import": func(c *Config) {
			c.AgentPlatform.ModelImport = true
			c.Cloud.Region = "eu-west-1"
		},
		"modelImport with the agent platform off": func(c *Config) {
			c.AgentPlatform.ModelImport = true
			c.AgentPlatform.Enable = boolPtr(false)
		},
		"cluster.version with a patch component": func(c *Config) { c.Cluster.Version = "1.36.2" },
		"cluster.version with a leading v":       func(c *Config) { c.Cluster.Version = "v1.36" },
	}
	for name, mutate := range cases {
		c := valid()
		mutate(c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: expected validation error, got nil", name)
		}
	}
}

// A well-formed CIDR allow-list validates; a malformed entry is rejected before it can be
// injected verbatim onto the public API endpoint. The empty allow-list (autodetect) is fine.
func TestValidate_EndpointAllowlistCIDRs(t *testing.T) {
	c := valid()
	c.Cluster.EndpointAllowlist = []string{"203.0.113.4/32", "10.0.0.0/16"}
	if err := c.Validate(); err != nil {
		t.Fatalf("a well-formed CIDR allow-list must validate: %v", err)
	}

	c.Cluster.EndpointAllowlist = []string{"203.0.113.4/32", "not-a-cidr"}
	if err := c.Validate(); err == nil {
		t.Fatal("a malformed allow-list entry must be rejected — it would otherwise land verbatim on the public API endpoint")
	}

	c.Cluster.EndpointAllowlist = nil
	if err := c.Validate(); err != nil {
		t.Fatalf("an empty allow-list (the autodetect case) must validate: %v", err)
	}
}

// The create-mode network levers mirror landing-zone's own preconditions so a bad
// combination fails in rackctl's config validation, in a second, rather than ~20 minutes
// into a tofu apply. A fully wired IPAM + transit-gateway + centralized-egress config is
// valid; each contradictory combination is rejected.
func TestValidate_NetworkLevers(t *testing.T) {
	// The whole chain, correctly wired, validates: IPAM pool + netmask, TGW on top of the
	// IPAM CIDR, centralized egress on top of the TGW.
	c := valid()
	c.Cluster.Network = ClusterNet{
		VPCCIDR:           defaultVPCCIDR, // left at default — the CIDR comes from the pool
		NATGateways:       1,
		IPAMPoolID:        "ipam-pool-0abc123",
		IPAMNetmaskLength: 18,
		TransitGatewayID:  "tgw-0abc123",
		CentralizedEgress: true,
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("a fully wired IPAM/TGW/centralized-egress config must validate: %v", err)
	}

	// Off (all levers empty) is the common day-0 case and must validate.
	c = valid()
	if err := c.Validate(); err != nil {
		t.Fatalf("the default network config (all levers off) must validate: %v", err)
	}

	reject := map[string]func(*ClusterNet){
		"centralized egress without a transit gateway": func(n *ClusterNet) {
			n.CentralizedEgress = true
		},
		"transit gateway without an IPAM pool": func(n *ClusterNet) {
			n.TransitGatewayID = "tgw-0abc123"
		},
		"ipam pool with a non-default vpc cidr": func(n *ClusterNet) {
			n.IPAMPoolID = "ipam-pool-0abc123"
			n.IPAMNetmaskLength = 16
			n.VPCCIDR = "10.20.0.0/16"
		},
		"ipam pool with a netmask below /16": func(n *ClusterNet) {
			n.IPAMPoolID = "ipam-pool-0abc123"
			n.IPAMNetmaskLength = 15
		},
		"ipam pool with a netmask above /20": func(n *ClusterNet) {
			n.IPAMPoolID = "ipam-pool-0abc123"
			n.IPAMNetmaskLength = 21
		},
		"ipam pool with no netmask": func(n *ClusterNet) {
			n.IPAMPoolID = "ipam-pool-0abc123" // IPAMNetmaskLength stays 0
		},
		"netmask set without an ipam pool": func(n *ClusterNet) {
			n.IPAMNetmaskLength = 18 // no pool to allocate from
		},
	}
	for name, mutate := range reject {
		t.Run(name, func(t *testing.T) {
			c := valid()
			mutate(&c.Cluster.Network)
			if err := c.Validate(); err == nil {
				t.Errorf("%s: expected validation error, got nil", name)
			}
		})
	}
}

// The model-import gate mirrors the runbook's regional precondition, so a staging bucket
// and an import role that no CreateModelImportJob could ever use fail in rackctl's
// validation in a second — rather than applying perfectly cleanly and being discovered
// dead by a human halfway through an import, which is what Terraform would do.
func TestValidate_ModelImport(t *testing.T) {
	// The supported case: the gate on, in a region where Custom Model Import runs.
	c := valid() // Default() puts the region at us-west-2
	c.AgentPlatform.ModelImport = true
	if err := c.Validate(); err != nil {
		t.Fatalf("modelImport in a Custom Model Import region must validate: %v", err)
	}

	c = valid()
	c.AgentPlatform.ModelImport = true
	c.Cloud.Region = "eu-west-1"
	if err := c.Validate(); err == nil {
		t.Error("modelImport outside a Custom Model Import region must be rejected — the component applies " +
			"cleanly there and is permanently unusable")
	}

	c = valid()
	c.AgentPlatform.ModelImport = true
	c.AgentPlatform.Enable = boolPtr(false)
	if err := c.Validate(); err == nil {
		t.Error("modelImport with the agent platform off must be rejected — nothing would consume the substrate")
	}

	// The region rule must bite ONLY when the gate is on. An org that runs in a region
	// without Custom Model Import must never be blocked by a feature it did not ask for.
	c = valid()
	c.Cloud.Region = "eu-west-1"
	if err := c.Validate(); err != nil {
		t.Fatalf("a region without Custom Model Import must validate when modelImport is off: %v", err)
	}
}

// ApplyDefaults must default the base network fields individually, never replace the
// whole ClusterNet — otherwise a lever set without a vpcCidr (the natural IPAM config,
// where the CIDR comes from the pool) would be silently wiped by the defaulting pass.
func TestApplyDefaults_PreservesNetworkLevers(t *testing.T) {
	c := &Config{Org: Org{Name: "acme"}}
	c.Cluster.Network = ClusterNet{
		IPAMPoolID:        "ipam-pool-0abc123",
		IPAMNetmaskLength: 16,
		TransitGatewayID:  "tgw-0abc123",
		CentralizedEgress: true,
		// vpcCidr deliberately left empty — the IPAM pool supplies the CIDR
	}
	c.ApplyDefaults()

	if c.Cluster.Network.IPAMPoolID != "ipam-pool-0abc123" {
		t.Errorf("ipamPoolId wiped by ApplyDefaults: %+v", c.Cluster.Network)
	}
	if c.Cluster.Network.TransitGatewayID != "tgw-0abc123" || !c.Cluster.Network.CentralizedEgress {
		t.Errorf("transitGatewayId/centralizedEgress wiped by ApplyDefaults: %+v", c.Cluster.Network)
	}
	if c.Cluster.Network.VPCCIDR != defaultVPCCIDR {
		t.Errorf("vpcCidr default = %q, want %q", c.Cluster.Network.VPCCIDR, defaultVPCCIDR)
	}
}

func TestApplyDefaults(t *testing.T) {
	c := &Config{Org: Org{Name: "acme"}}
	c.ApplyDefaults()
	if c.Cloud.Region != "us-west-2" {
		t.Errorf("region default = %q, want us-west-2", c.Cloud.Region)
	}
	if c.Environment != EnvDev {
		t.Errorf("environment default = %q, want development", c.Environment)
	}
	if c.Org.GitOps.EKSGitopsRepo != "github.com/acme/eks-gitops" {
		t.Errorf("eksGitopsRepo = %q, want derived from org", c.Org.GitOps.EKSGitopsRepo)
	}
}

// The tier defaults to full and is a closed enum.
//
// Default full is deliberate: a rackctl-installed platform is the full agent platform, and
// it matches what all four committed cluster-bootstrap leaves already pin. floor is the
// opt-down for a cluster that should not carry AMP/AMG cost.
//
// The enum must be closed because the value is published as the observability/tier label on
// the ArgoCD cluster Secret, which the tier-aware eks-gitops ApplicationSets select on. A
// typo does not fail anything — it produces a label that matches no generator, so the
// cluster comes up with the OTel node agent and nothing else. That is the quietest possible
// failure, which is exactly why it is rejected here.
func TestValidate_ObservabilityTier(t *testing.T) {
	if got := Default().Observability.Tier; got != TierFull {
		t.Errorf("the default tier must be full, got %q", got)
	}

	c := valid()
	if c.Observability.Tier != TierFull {
		t.Errorf("ApplyDefaults must fill the tier, got %q", c.Observability.Tier)
	}

	for _, tier := range []ObservabilityTier{TierFull, TierFloor} {
		c := valid()
		c.Observability.Tier = tier
		if err := c.Validate(); err != nil {
			t.Errorf("tier %q must validate: %v", tier, err)
		}
		if got := c.FullObservability(); got != (tier == TierFull) {
			t.Errorf("FullObservability() = %v for tier %q", got, tier)
		}
	}

	for _, bad := range []ObservabilityTier{"Full", "FULL", "none", "amp", "true"} {
		c := valid()
		c.Observability.Tier = bad
		if err := c.Validate(); err == nil {
			t.Errorf("tier %q must be rejected — an unrecognised label matches no ApplicationSet "+
				"generator, so the cluster silently gets the node agent and nothing else", bad)
		}
	}
}

// An empty tier is filled by ApplyDefaults, never left to reach Validate as a blank label.
func TestApplyDefaults_FillsTheObservabilityTier(t *testing.T) {
	c := &Config{Org: Org{Name: "acme"}}
	c.ApplyDefaults()
	if c.Observability.Tier != TierFull {
		t.Errorf("tier = %q, want full — a blank label matches no generator", c.Observability.Tier)
	}
}
