// Package config defines the rackctl.yaml schema that describes a full-provision
// nanohype platform, plus loading, defaulting, and validation. The shape is
// derived directly from landing-zone's account.hcl, the eks-fleet Cluster CR,
// and the eks-agent-platform tenant chart.
package config

import (
	"fmt"
	"net"
	"regexp"
	"strings"
)

// rfc1123Label is the shape a cluster base name must take (a lowercase DNS label):
// it becomes part of the EKS cluster name and the AWS resource names derived from it.
var rfc1123Label = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,28}[a-z0-9])?$`)

// Provider is the target cloud. v1 supports AWS only.
type Provider string

const (
	ProviderAWS   Provider = "aws"
	ProviderAzure Provider = "azure" // reserved: no aks-gitops catalog exists yet
)

// Environment selects the eks-gitops overlay and default sizing.
type Environment string

const (
	EnvDev        Environment = "development"
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
	// Stored bare ("github.com/<org>/eks-gitops"); use GitURL for the clone/ArgoCD form.
	EKSGitopsRepo string `json:"eksGitopsRepo"`
	// ClustersRepo backs eks-fleet Cluster CRs (only with controlPlane.eksFleet).
	ClustersRepo string `json:"clustersRepo,omitempty"`
	// TenantsRepo backs rendered tenant charts (only with controlPlane.portal).
	TenantsRepo string `json:"tenantsRepo,omitempty"`
}

// GitURL renders EKSGitopsRepo as the clonable https URL ArgoCD wants
// ("github.com/acme/eks-gitops" -> "https://github.com/acme/eks-gitops.git").
//
// This is the value cluster-bootstrap hands to the app-of-apps Application and
// publishes on the ArgoCD cluster Secret, from which every ApplicationSet in the
// catalog templates its own source. It must therefore point at the ORG'S FORK, not
// at the upstream catalog: landing-zone's gitops_repo_url used to default to
// nanohype/eks-gitops, and because nothing passed a value, every install silently
// synced from upstream main while the org's fork sat unread.
//
// Returns "" for an empty repo so callers can detect the unset case rather than
// emit a URL like "https://.git".
func (g OrgGitOps) GitURL() string {
	if g.EKSGitopsRepo == "" {
		return ""
	}
	u := g.EKSGitopsRepo
	if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "git@") {
		u = "https://" + u
	}
	if !strings.HasSuffix(u, ".git") {
		u += ".git"
	}
	return u
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
	// Name is the cluster base; the EKS cluster is <environment>-<Name> (see
	// Config.ClusterName). Required and unique per (account, region, environment) —
	// no default, because a shared default collides the moment a second cluster lands
	// in one account and environment. Must not equal the environment token. Mirrors
	// eks-fleet's Cluster.spec.clusterName and landing-zone's var.cluster_name.
	Name                 string `json:"name"`
	Version              string `json:"version"`
	EndpointPublicAccess bool   `json:"endpointPublicAccess"` // prod should be false (needs bastion/VPN)
	// EndpointAllowlist is the set of CIDR blocks permitted to reach the public EKS API
	// endpoint. It rides TF_VAR_cluster_endpoint_public_access_cidrs into landing-zone's
	// cluster component, whose committed tree is private-by-default and fail-closed: a
	// public endpoint with no allow-list is rejected at plan time — there is no 0.0.0.0/0
	// fallback. When EndpointPublicAccess is true and this is empty, the cluster phase
	// auto-detects the operator's public egress IP and scopes the endpoint to <ip>/32.
	// An explicit allow-list always wins over autodetection.
	EndpointAllowlist []string   `json:"endpointAllowlist,omitempty"`
	SystemNodes       NodeGroup  `json:"systemNodes"`
	Network           ClusterNet `json:"network"`
	TTLDays           int        `json:"ttlDays"` // eks-fleet auto-reap; 0 = persistent
}

// ClusterName is the resolved EKS cluster name, <environment>-<cluster.name> — the
// single source of truth every component derives its resources from. landing-zone's
// cluster module composes the same string from var.environment + var.cluster_name, so
// rackctl passes cluster.name as TF_VAR_cluster_name and reads this value back for
// describe-cluster / kubeconfig / reap.
func (c *Config) ClusterName() string {
	return string(c.Environment) + "-" + c.Cluster.Name
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
	// Enable installs the agent platform. Omitted (nil) defaults to true — it is
	// the whole point of the platform; set it false to explicitly opt out.
	Enable               *bool      `json:"enable,omitempty"`
	BedrockModelFamilies []string   `json:"bedrockModelFamilies"`
	Compliance           Compliance `json:"compliance"`
}

// Enabled reports whether the agent platform should be installed. An omitted
// agentPlatform block (nil) defaults to enabled.
func (a AgentPlatform) Enabled() bool { return a.Enable == nil || *a.Enable }

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

func boolPtr(b bool) *bool { return &b }

// Default returns a Config populated with the sane development defaults.
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
			Enable:               boolPtr(true),
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
	if c.AgentPlatform.Enabled() && len(c.AgentPlatform.BedrockModelFamilies) == 0 {
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
		errs = append(errs, fmt.Sprintf("environment must be development|staging|production, got %q", c.Environment))
	}
	switch {
	case c.Cluster.Name == "":
		errs = append(errs, "cluster.name is required (the cluster base; the EKS cluster is <environment>-<name>)")
	case !rfc1123Label.MatchString(c.Cluster.Name):
		errs = append(errs, fmt.Sprintf("cluster.name %q must be a lowercase RFC-1123 label", c.Cluster.Name))
	case len(c.Cluster.Name) > 12:
		errs = append(errs, fmt.Sprintf("cluster.name %q must be <= 12 chars: the derived <environment>-<name> feeds cluster-scoped S3/IAM names; the tightest (agent-iam's account+region-qualified model-artifacts bucket) fits within S3's 63-char limit in us-west-2", c.Cluster.Name))
	case c.Cluster.Name == string(c.Environment):
		errs = append(errs, fmt.Sprintf("cluster.name must not equal environment (the cluster name would double, e.g. %[1]s-%[1]s)", c.Environment))
	}
	if c.Environment == EnvProduction && c.Cluster.EndpointPublicAccess {
		errs = append(errs, "cluster.endpointPublicAccess should be false for production (requires bastion/VPN)")
	}
	// Every allow-list entry must parse as a CIDR block: a malformed entry (a bare IP, a
	// typo'd mask) would otherwise fail landing-zone's plan late and opaquely, or worse be
	// injected verbatim onto the control plane's public endpoint. Catch it here. Entries are
	// validated whenever present, regardless of endpointPublicAccess — an allow-list that is
	// wrong is wrong even while it is unused.
	for i, cidr := range c.Cluster.EndpointAllowlist {
		if _, _, err := net.ParseCIDR(strings.TrimSpace(cidr)); err != nil {
			errs = append(errs, fmt.Sprintf("cluster.endpointAllowlist[%d] %q must be a CIDR block, e.g. 203.0.113.4/32", i, cidr))
		}
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
