// Package config defines the rackctl.yaml schema that describes a full-provision
// nanohype platform, plus loading, defaulting, and validation. The shape is
// derived directly from landing-zone's account.hcl, the eks-fleet Cluster CR,
// and the eks-agent-platform tenant chart.
package config

import (
	"fmt"
	"strings"
)

// Provider is the target cloud. v1 supports AWS only.
type Provider string

const (
	ProviderAWS   Provider = "aws"
	ProviderAzure Provider = "azure" // reserved: no aks-gitops catalog exists yet
)

// Environment selects the eks-gitops overlay and default sizing.
type Environment string

const (
	EnvDev        Environment = "dev"
	EnvStaging    Environment = "staging"
	EnvProduction Environment = "production"
)

// Config is the full rackctl.yaml document.
type Config struct {
	Org           Org           `json:"org"`
	Cloud         Cloud         `json:"cloud"`
	Environment   Environment   `json:"environment"`
	Cluster       Cluster       `json:"cluster"`
	Quotas        Quotas        `json:"quotas"`
	Addons        Addons        `json:"addons"`
	DNS           *DNS          `json:"dns,omitempty"`
	AgentPlatform AgentPlatform `json:"agentPlatform"`
	ControlPlane  ControlPlane  `json:"controlPlane"`
	FirstTenant   *FirstTenant  `json:"firstTenant,omitempty"`
}

type Org struct {
	Name   string    `json:"name"`
	GitOps OrgGitOps `json:"gitops"`
}

type OrgGitOps struct {
	// EKSGitopsRepo is the operator's fork of nanohype/eks-gitops (the ArgoCD addon catalog).
	EKSGitopsRepo string `json:"eksGitopsRepo"`
	// ClustersRepo backs eks-fleet Cluster CRs (only with controlPlane.eksFleet).
	ClustersRepo string `json:"clustersRepo,omitempty"`
	// TenantsRepo backs rendered tenant charts (only with controlPlane.portal).
	TenantsRepo string `json:"tenantsRepo,omitempty"`
}

type Cloud struct {
	Provider       Provider        `json:"provider"`
	AccountID      string          `json:"accountId"`
	Region         string          `json:"region"`
	Profile        string          `json:"profile"` // AWS SSO profile
	IdentityCenter *IdentityCenter `json:"identityCenter,omitempty"`
}

type IdentityCenter struct {
	Manage    bool   `json:"manage"`
	AdminUser string `json:"adminUser,omitempty"`
}

type Cluster struct {
	Version              string     `json:"version"`
	EndpointPublicAccess bool       `json:"endpointPublicAccess"` // prod should be false (needs bastion/VPN)
	SystemNodes          NodeGroup  `json:"systemNodes"`
	Network              ClusterNet `json:"network"`
	TTLDays              int        `json:"ttlDays"` // eks-fleet auto-reap; 0 = persistent
}

type NodeGroup struct {
	InstanceTypes []string `json:"instanceTypes"`
	MinSize       int      `json:"minSize"`
	MaxSize       int      `json:"maxSize"`
	DesiredSize   int      `json:"desiredSize"`
}

type ClusterNet struct {
	VPCCIDR     string `json:"vpcCidr"`
	NATGateways int    `json:"natGateways"`
}

type Quotas struct {
	AutoRequest bool `json:"autoRequest"` // file L-1216C47A (EC2 vCPU) etc. before provisioning
	VCPU        int  `json:"vcpu"`
}

type Addons struct {
	Observability bool `json:"observability"` // managed-monitoring (AMP+AMG)
	Druid         bool `json:"druid"`
	Accelerators  bool `json:"accelerators"` // gpu-operator / neuron
}

type DNS struct {
	HostedZone string `json:"hostedZone"`
}

type AgentPlatform struct {
	Enable               bool       `json:"enable"`
	BedrockModelFamilies []string   `json:"bedrockModelFamilies"`
	Compliance           Compliance `json:"compliance"`
}

type Compliance struct {
	SOC2  bool `json:"soc2"`
	HIPAA bool `json:"hipaa"`
}

type ControlPlane struct {
	EKSFleet bool `json:"eksFleet"` // Crossplane cluster control plane (multi-cluster)
	Portal   bool `json:"portal"`   // day-2 operator UI
}

type FirstTenant struct {
	Name             string `json:"name"`
	Persona          string `json:"persona"`
	Tenant           string `json:"tenant"`
	MonthlyBudgetUSD int    `json:"monthlyBudgetUsd"`
}

// Default returns a Config populated with the sane dev defaults.
func Default() *Config {
	return &Config{
		Cloud:       Cloud{Provider: ProviderAWS, Region: "us-west-2"},
		Environment: EnvDev,
		Cluster: Cluster{
			Version:              "1.35",
			EndpointPublicAccess: true,
			SystemNodes:          NodeGroup{InstanceTypes: []string{"m7g.xlarge"}, MinSize: 2, MaxSize: 6, DesiredSize: 2},
			Network:              ClusterNet{VPCCIDR: "10.0.0.0/16", NATGateways: 1},
		},
		Quotas: Quotas{AutoRequest: true, VCPU: 256},
		Addons: Addons{Observability: true},
		AgentPlatform: AgentPlatform{
			Enable:               true,
			BedrockModelFamilies: []string{"anthropic", "amazon-nova"},
			Compliance:           Compliance{SOC2: true},
		},
	}
}

// ApplyDefaults fills unset fields on a loaded config.
func (c *Config) ApplyDefaults() {
	d := Default()
	if c.Cloud.Provider == "" {
		c.Cloud.Provider = d.Cloud.Provider
	}
	if c.Cloud.Region == "" {
		c.Cloud.Region = d.Cloud.Region
	}
	if c.Environment == "" {
		c.Environment = d.Environment
	}
	if c.Cluster.Version == "" {
		c.Cluster.Version = d.Cluster.Version
	}
	if len(c.Cluster.SystemNodes.InstanceTypes) == 0 {
		c.Cluster.SystemNodes = d.Cluster.SystemNodes
	}
	if c.Cluster.Network.VPCCIDR == "" {
		c.Cluster.Network = d.Cluster.Network
	}
	if c.Quotas.VCPU == 0 {
		c.Quotas = d.Quotas
	}
	if c.AgentPlatform.Enable && len(c.AgentPlatform.BedrockModelFamilies) == 0 {
		c.AgentPlatform.BedrockModelFamilies = d.AgentPlatform.BedrockModelFamilies
	}
	if c.Org.Name != "" && c.Org.GitOps.EKSGitopsRepo == "" {
		c.Org.GitOps.EKSGitopsRepo = fmt.Sprintf("github.com/%s/eks-gitops", c.Org.Name)
	}
}

// Validate checks required fields and v1 constraints.
func (c *Config) Validate() error {
	var errs []string
	if c.Org.Name == "" {
		errs = append(errs, "org.name is required")
	}
	if c.Cloud.Provider != ProviderAWS {
		errs = append(errs, fmt.Sprintf("cloud.provider must be %q (v1 supports AWS only)", ProviderAWS))
	}
	switch len(c.Cloud.AccountID) {
	case 0:
		errs = append(errs, "cloud.accountId is required")
	case 12:
	default:
		errs = append(errs, "cloud.accountId must be a 12-digit AWS account id")
	}
	if c.Cloud.Region == "" {
		errs = append(errs, "cloud.region is required")
	}
	if c.Cloud.Profile == "" {
		errs = append(errs, "cloud.profile is required (AWS SSO profile)")
	}
	switch c.Environment {
	case EnvDev, EnvStaging, EnvProduction:
	default:
		errs = append(errs, fmt.Sprintf("environment must be dev|staging|production, got %q", c.Environment))
	}
	if c.Environment == EnvProduction && c.Cluster.EndpointPublicAccess {
		errs = append(errs, "cluster.endpointPublicAccess should be false for production (requires bastion/VPN)")
	}
	if c.ControlPlane.EKSFleet && c.Org.GitOps.ClustersRepo == "" {
		errs = append(errs, "org.gitops.clustersRepo is required when controlPlane.eksFleet is true")
	}
	if c.ControlPlane.Portal && c.Org.GitOps.TenantsRepo == "" {
		errs = append(errs, "org.gitops.tenantsRepo is required when controlPlane.portal is true")
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid config:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}
