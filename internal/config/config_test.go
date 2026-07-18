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
