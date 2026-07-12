package config

import "testing"

func valid() *Config {
	c := Default()
	c.Org.Name = "acme"
	c.Cloud.AccountID = "111111111111"
	c.Cloud.Profile = "workload-dev"
	c.ApplyDefaults()
	return c
}

func TestValidate(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatalf("valid config errored: %v", err)
	}

	cases := map[string]func(*Config){
		"missing org.name":         func(c *Config) { c.Org.Name = "" },
		"short account id":         func(c *Config) { c.Cloud.AccountID = "123" },
		"non-aws provider":         func(c *Config) { c.Cloud.Provider = ProviderAzure },
		"bad environment":          func(c *Config) { c.Environment = "qa" },
		"prod public endpoint":     func(c *Config) { c.Environment = EnvProduction; c.Cluster.EndpointPublicAccess = true },
		"eksFleet no clustersRepo": func(c *Config) { c.ControlPlane.EKSFleet = true },
		"portal no tenantsRepo":    func(c *Config) { c.ControlPlane.Portal = true },
	}
	for name, mutate := range cases {
		c := valid()
		mutate(c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: expected validation error, got nil", name)
		}
	}
}

func TestApplyDefaults(t *testing.T) {
	c := &Config{Org: Org{Name: "acme"}}
	c.ApplyDefaults()
	if c.Cloud.Region != "us-west-2" {
		t.Errorf("region default = %q, want us-west-2", c.Cloud.Region)
	}
	if c.Environment != EnvDev {
		t.Errorf("environment default = %q, want dev", c.Environment)
	}
	if c.Org.GitOps.EKSGitopsRepo != "github.com/acme/eks-gitops" {
		t.Errorf("eksGitopsRepo = %q, want derived from org", c.Org.GitOps.EKSGitopsRepo)
	}
}
