// Package config defines the rackctl.yaml schema that describes a full-provision
// nanohype platform, plus loading, defaulting, and validation. The shape is
// derived directly from landing-zone's account.hcl, the eks-fleet Cluster CR,
// and the eks-agent-platform tenant chart.
package config

import (
	"fmt"
	"net"
	"regexp"
	"slices"
	"strings"
)

// rfc1123Label is the shape a cluster base name must take (a lowercase DNS label):
// it becomes part of the EKS cluster name and the AWS resource names derived from it.
var rfc1123Label = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,28}[a-z0-9])?$`)

// defaultVPCCIDR is the literal CIDR a create-mode VPC uses when it is not drawn from an
// IPAM pool. It doubles as the sentinel for "unset" in the ipamPoolId ⇄ vpcCidr mutual
// exclusion: an IPAM-allocated VPC leaves vpcCidr at this default (the CIDR comes from the
// pool, not the literal). landing-zone's network component defaults var.vpc_cidr to the
// same value, so the two agree on what "not overridden" means.
const defaultVPCCIDR = "10.0.0.0/16"

// bedrockCustomModelImportRegions are the regions Bedrock Custom Model Import runs in.
//
// This mirrors eks-agent-platform/docs/runbooks/import-open-weight-model.md
// ("Prerequisites"). An imported model is an account+region resource that must be
// imported into the region it is served from, so a staging bucket and an import role
// provisioned anywhere else are permanently unusable — and, critically, provisioning
// them there APPLIES CLEANLY. Terraform has no opinion about which regions Bedrock
// offers the feature in; the failure surfaces much later, at CreateModelImportJob, to
// a human following the runbook.
//
// The list is an AWS-side moving target. If AWS adds a region, update the runbook
// first — it is the source of truth — and then this list.
var bedrockCustomModelImportRegions = []string{"us-west-2", "us-east-1", "us-east-2", "eu-central-1"}

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
	Observability Observability `json:"observability"`
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

// NetworkMode selects whether this platform owns its VPC or participates in one it does not.
type NetworkMode string

const (
	// ModeCreate builds the VPC, subnets, endpoints and egress. The default, and what a
	// day-0 hub normally wants.
	ModeCreate NetworkMode = "create"
	// ModeAdopt participates in a VPC someone else owns — a shared VPC in this account, or
	// one shared in over AWS RAM. landing-zone's network component builds nothing in this
	// mode: it resolves the VPC, subnets, CIDR and AZs from the adopt inputs and re-exports
	// them through the same outputs, so a consuming cluster wires identically either way.
	ModeAdopt NetworkMode = "adopt"
)

// adoptMinPrivateSubnets is landing-zone's `max_azs` default (3), which its adopt preflight
// asserts the adopted private subnets span. rackctl cannot know a subnet's AZ without an AWS
// call, but N subnets can never cover more than N zones — so requiring at least this many
// distinct private subnet IDs is a sound necessary condition, checkable from the config alone.
const adoptMinPrivateSubnets = 3

type ClusterNet struct {
	VPCCIDR     string `json:"vpcCidr"`
	NATGateways int    `json:"natGateways"`

	// Mode selects create (default) or adopt. See NetworkMode.
	//
	// The committed workload leaves are all create-by-omission, and landing-zone's only
	// adopt example lives outside every workload account (live/aws/reference-adopt/) because
	// it wires the adopt inputs from a `dependency` on another account's state — and an
	// environment that depends on another account's state cannot be brought up on its own.
	//
	// rackctl has no such problem: it has an operator, who knows their VPC id. It injects
	// network_mode and the adopt inputs as TF_VAR_*, which beat a leaf's `inputs`, so there
	// is no dependency block, no cross-account state read, and no need for that tree. Which
	// means rackctl can offer adopt in a workload environment precisely where the committed
	// tree cannot.
	//
	// This was already reachable before it was a field, and that is why it is one:
	// internal/exec passes os.Environ() into every terragrunt invocation, so an operator
	// could always export TF_VAR_network_mode=adopt and have it take effect — unvalidated,
	// undocumented, and with none of the guards below. On staging and production it would
	// also have failed, because those leaves pin create-mode values that adopt rejects.
	Mode NetworkMode `json:"mode,omitempty"`
	// AdoptVPCID is the VPC to participate in. Required under adopt, rejected under create.
	AdoptVPCID string `json:"adoptVpcId,omitempty"`
	// AdoptPrivateSubnetIDs are the private subnets in the adopted VPC — where nodes run.
	// Required and non-empty under adopt, rejected under create. Must span at least
	// adoptMinPrivateSubnets distinct zones, which landing-zone asserts at plan time.
	AdoptPrivateSubnetIDs []string `json:"adoptPrivateSubnetIds,omitempty"`
	// AdoptPublicSubnetIDs are the public subnets, and empty is VALID — a private-only
	// cluster is a supported adopt shape. Rejected under create.
	//
	// Leaving it empty has a consequence worth knowing: cluster-bootstrap publishes the
	// public subnet list as an annotation for the Kyverno rule that injects load-balancer
	// subnets, and that rule guards on a non-empty list. So an internet-facing Service or
	// Ingress on a private-only adopt cluster gets no subnet annotation and does not
	// provision. Internal load balancers are unaffected.
	AdoptPublicSubnetIDs []string `json:"adoptPublicSubnetIds,omitempty"`

	// The four fields below are the create-mode network levers. They opt a day-0 hub out
	// of the committed live tree's plain literal-CIDR VPC with local NAT and into the
	// org's IPAM / transit-gateway topology. Each rides a TF_VAR_* onto landing-zone's
	// network component at apply time (see internal/phases/network.go), same idiom as
	// TF_VAR_cluster_name — the committed tree stays generic, rackctl layers the per-run
	// choice over it. All default off (empty / 0 / false), and all four are rejected under
	// adopt: a VPC this platform does not own has its CIDR, its egress and its
	// transit-gateway attachment decided by the owner.
	//
	// Validate mirrors landing-zone's own preconditions so a contradictory combination
	// fails here, in a second, instead of seconds-to-minutes into a tofu run.

	// IPAMPoolID draws the VPC CIDR from an IPAM pool instead of the literal VPCCIDR
	// (cross-account, the org IPAM env sub-pool shared in over RAM). Empty = literal
	// allocation. Mutually exclusive with a non-default VPCCIDR; requires
	// IPAMNetmaskLength.
	IPAMPoolID string `json:"ipamPoolId,omitempty"`
	// IPAMNetmaskLength is the netmask of the CIDR allocated from IPAMPoolID (e.g. 16 for
	// a /16). Between 16 and 20 when a pool is set — subnets are carved 8 bits smaller
	// than the VPC block, so a /20 base is the smallest that still yields AWS's /28
	// minimum subnet. 0 (default) when no pool is set.
	IPAMNetmaskLength int `json:"ipamNetmaskLength,omitempty"`
	// TransitGatewayID attaches the VPC to a transit gateway and routes 10.0.0.0/8 to it,
	// so the VPC reaches the rest of the org's address space. Empty = no attachment,
	// local NAT egress only. Requires an IPAM-allocated CIDR (IPAMPoolID) — a TGW route
	// domain needs non-overlapping, IPAM-governed prefixes.
	TransitGatewayID string `json:"transitGatewayId,omitempty"`
	// CentralizedEgress routes the private default route (0.0.0.0/0) through the transit
	// gateway to a central egress VPC instead of a local NAT gateway (zero NAT gateways).
	// Requires TransitGatewayID — there is nothing to route egress to without one.
	CentralizedEgress bool `json:"centralizedEgress,omitempty"`
}

type Quotas struct {
	AutoRequest bool `json:"autoRequest"` // file L-1216C47A (EC2 vCPU) etc. before provisioning
	VCPU        int  `json:"vcpu"`
}

type Addons struct {
	Druid        bool `json:"druid"`
	Accelerators bool `json:"accelerators"` // gpu-operator / neuron
}

// ObservabilityTier selects which observability substrate a cluster runs. It mirrors
// landing-zone's cluster-bootstrap var.observability_tier, which publishes it as the
// `observability/tier` label on the ArgoCD cluster Secret — and eight eks-gitops
// ApplicationSet generators select on that label.
type ObservabilityTier string

const (
	// TierFloor is the provider-native tier: the amazon-cloudwatch-observability addon
	// publishes ContainerInsights metrics, and the OTel gateway exports metrics as CloudWatch
	// EMF, logs to CloudWatch Logs, and traces to AWS X-Ray. No signal is dropped — floor is
	// a different backend, not a smaller one.
	TierFloor ObservabilityTier = "floor"
	// TierFull is floor plus the in-cluster LGTM stack (Loki, Tempo, kube-state-metrics,
	// grafana-operator) and Amazon Managed Prometheus / Grafana.
	TierFull ObservabilityTier = "full"
)

// Observability is the cluster's observability substrate.
//
// One field, deliberately, because the two knobs this replaces could express a state that
// cannot work. landing-zone's cluster-bootstrap takes `observability_tier` and
// `enable_managed_monitoring` as INDEPENDENT variables with nothing relating them, so
// `tier = full` with no managed-monitoring is representable — and rackctl's old
// `addons.observability: false` produced exactly that, because every committed leaf pins
// tier=full and rackctl overrode only the flag.
//
// What breaks, concretely: the full-tier OTel gateway mounts AMP_REMOTE_WRITE_URL from a
// Kubernetes Secret that external-secrets syncs out of AWS SECRETS MANAGER — the
// `<cluster>-managed-monitoring-endpoints` entry, through the aws-secrets-manager
// ClusterSecretStore. That is not SSM, and the distinction matters to whoever debugs it: the
// managed-monitoring component publishes three SSM parameters AND that Secrets Manager entry,
// so grepping SSM tells you nothing about this failure. The loud symptom comes first — the
// gateway pod stuck in CreateContainerConfigError because the Secret it mounts does not
// exist — with the ExternalSecret's SecretSyncedError behind it.
//
// The invariant is cross-root and therefore unexpressible as a Terraform validation:
// tier=full REQUIRES the managed-monitoring component to have applied, because that component
// is the only thing that writes that Secrets Manager entry. No variable block can see whether
// another root ran. rackctl can — it is the thing that decides — so the tier is what gates
// applying the component, and the incoherent combination stops being expressible at all.
type Observability struct {
	// Tier defaults to full. A rackctl-installed platform is the full agent platform; floor
	// is the deliberate opt-down for a cluster that should not carry AMP/AMG cost. Floor is
	// not free either — its cost is EMF custom-metric cardinality, and that lever lives in the
	// eks-gitops values file rather than here.
	//
	// Treat the tier as a day-0 decision for a given cluster. rackctl injects it
	// unconditionally, and an ambient TF_VAR overrides the value a leaf pinned, so re-running
	// init with the tier changed is not a no-op: full→floor prunes Loki, Tempo,
	// grafana-operator and the dashboards, with a telemetry gap while it happens. See
	// eks-gitops/docs/runbooks/observability-tier.md before flipping one on a live cluster.
	Tier ObservabilityTier `json:"tier"`
}

// FullObservability reports whether this cluster runs the full tier — and therefore whether
// managed-monitoring is applied for it.
func (c *Config) FullObservability() bool { return c.Observability.Tier == TierFull }

type DNS struct {
	HostedZone string `json:"hostedZone"`
}

type AgentPlatform struct {
	// Enable installs the agent platform. Omitted (nil) defaults to true — it is
	// the whole point of the platform; set it false to explicitly opt out.
	Enable               *bool    `json:"enable,omitempty"`
	BedrockModelFamilies []string `json:"bedrockModelFamilies"`

	// ModelImport applies landing-zone's model-import component: the substrate Bedrock
	// Custom Model Import needs for this environment, and nothing else. That is the S3
	// staging bucket <environment>-<account>-<region>-model-import where Hugging Face-format
	// weights land, the IAM service role model-import-<environment>-<region> that Bedrock
	// assumes to read them during a CreateModelImportJob, and two SSM discovery parameters
	// under /eks-agent-platform/<environment>/model-import/.
	//
	// It imports NO model. Importing is a deliberate, infrequent, account-level act run
	// out of band by a human (eks-agent-platform/docs/runbooks/import-open-weight-model.md),
	// and rackctl deliberately does not automate it: the Concurrent-model-import-jobs
	// quota is a hard 1 and is not adjustable, and a freshly created import role fails
	// CreateModelImportJob with the misleading "The provided role ARN is invalid" for
	// several minutes while its trust propagates. Neither fits a day-0 installer's
	// failure model — but pre-provisioning the substrate takes that propagation window
	// off the human's path later, which is exactly what a day-0 installer is for.
	//
	// Off by default: it is per-environment substrate an org may never want. It is torn
	// down with everything else — landing-zone scopes the bucket and role by environment
	// and gives them a teardown posture, so nothing here has to outlive its cluster.
	ModelImport bool `json:"modelImport"`

	Compliance Compliance `json:"compliance"`
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
			Network:              ClusterNet{VPCCIDR: defaultVPCCIDR, NATGateways: 1},
		},
		Quotas:        Quotas{AutoRequest: true, VCPU: 256},
		Observability: Observability{Tier: TierFull},
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
	// Default the two base fields individually, never the whole struct — replacing
	// ClusterNet wholesale would wipe an IPAM/transit-gateway/egress lever set without a
	// vpcCidr, which is exactly the natural IPAM config (the CIDR comes from the pool, so
	// vpcCidr is left at its default).
	if c.Cluster.Network.VPCCIDR == "" {
		c.Cluster.Network.VPCCIDR = d.Cluster.Network.VPCCIDR
	}
	if c.Cluster.Network.NATGateways == 0 {
		c.Cluster.Network.NATGateways = d.Cluster.Network.NATGateways
	}
	// Default the tier field itself, not the whole struct — same rule as ClusterNet below:
	// replacing a sub-struct wholesale is how a deliberately-set sibling field gets wiped.
	if c.Observability.Tier == "" {
		c.Observability.Tier = d.Observability.Tier
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
	// Network mode, then the levers. Mirrors landing-zone's own variable validations so a
	// contradictory combination fails in a second rather than seconds-to-minutes into a tofu
	// run, and adds the checks landing-zone cannot make from a variable block.
	n := c.Cluster.Network
	errs = append(errs, validateNetworkMode(n)...)
	switch {
	case n.Adopt():
		// The create-mode relationship checks below are vacuous under adopt: every lever they
		// relate has already been rejected outright. Running them anyway produces a second,
		// misleading error — "ipamNetmaskLength must be between 16 and 20" alongside
		// "ipamPoolId does not apply under adopt" reads as though setting a netmask would
		// help. landing-zone emits one error here for the same reason: its
		// ipam_netmask_length validation references only ipam_pool_id, never network_mode, so
		// a pool set under adopt fails on ipam_pool_id alone.
	case n.IPAMPoolID == "":
		// No IPAM pool ⇒ literal allocation, which must not carry a netmask.
		if n.IPAMNetmaskLength != 0 {
			errs = append(errs, fmt.Sprintf("cluster.network.ipamNetmaskLength (%d) applies only with cluster.network.ipamPoolId set — leave it 0 for a literal-CIDR VPC", n.IPAMNetmaskLength))
		}
	default:
		// A VPC CIDR comes from exactly one source: an IPAM pool OR a literal vpcCidr,
		// never both. With a pool set, vpcCidr must stay at its default.
		if n.VPCCIDR != "" && n.VPCCIDR != defaultVPCCIDR {
			errs = append(errs, fmt.Sprintf("cluster.network.ipamPoolId and a non-default cluster.network.vpcCidr (%q) are mutually exclusive — with an IPAM pool the CIDR is drawn from the pool, so leave vpcCidr unset", n.VPCCIDR))
		}
		// An IPAM allocation needs a netmask, bounded 16–20: subnets are carved 8 bits
		// smaller than the VPC block, so a /20 base is the smallest that still yields
		// AWS's /28 minimum subnet.
		if n.IPAMNetmaskLength < 16 || n.IPAMNetmaskLength > 20 {
			errs = append(errs, fmt.Sprintf("cluster.network.ipamNetmaskLength must be between 16 and 20 when cluster.network.ipamPoolId is set, got %d — subnets are carved 8 bits smaller than the VPC block, so a base longer than /20 falls below AWS's /28 minimum", n.IPAMNetmaskLength))
		}
	}
	if !n.Adopt() {
		// A transit-gateway attachment requires an IPAM-allocated CIDR — a raw literal vpcCidr
		// can overlap another attached VPC and break TGW routing.
		if n.TransitGatewayID != "" && n.IPAMPoolID == "" {
			errs = append(errs, "cluster.network.transitGatewayId requires an IPAM-allocated CIDR (set cluster.network.ipamPoolId) — a literal vpcCidr can overlap another attached VPC and break transit-gateway routing")
		}
		// Centralized egress has nothing to route the default egress to without a TGW.
		if n.CentralizedEgress && n.TransitGatewayID == "" {
			errs = append(errs, "cluster.network.centralizedEgress requires cluster.network.transitGatewayId — there is nothing to route the private default egress to without a transit gateway")
		}
	}
	switch c.Observability.Tier {
	case TierFloor, TierFull:
	default:
		errs = append(errs, fmt.Sprintf("observability.tier must be %q or %q, got %q — the value is published as "+
			"the observability/tier label on the ArgoCD cluster Secret, and every tier-aware eks-gitops "+
			"ApplicationSet either selects on it or derives a value from it. An unrecognised tier matches no "+
			"generator at all, so the cluster comes up with the OTel node agent and nothing else",
			TierFull, TierFloor, c.Observability.Tier))
	}
	// model-import. Both rules exist for the same reason the network levers above do:
	// the component APPLIES CLEANLY in either wrong configuration and is silently
	// useless afterwards, so the only place the mistake can be caught cheaply is here.
	if c.AgentPlatform.ModelImport {
		if !c.AgentPlatform.Enabled() {
			errs = append(errs, "agentPlatform.modelImport requires the agent platform, but agentPlatform.enable is false — "+
				"the model-import substrate exists so a ModelGateway route can reference an imported-model ARN, and its "+
				"discovery parameters live under /eks-agent-platform/<environment>/model-import/. With the platform off there is "+
				"no "+
				"operator to reconcile that route, so the staging bucket, the import role and the two SSM parameters would "+
				"be account substrate nothing on this platform can consume")
		}
		if !slices.Contains(bedrockCustomModelImportRegions, c.Cloud.Region) {
			errs = append(errs, fmt.Sprintf("agentPlatform.modelImport is set but cloud.region %q is not one of %s — Bedrock "+
				"Custom Model Import runs only in those regions, and an imported model is an account+region resource that "+
				"must be imported into the region it is served from. Applying model-import elsewhere SUCCEEDS in Terraform "+
				"and silently produces a staging bucket and an import service role that no CreateModelImportJob can ever "+
				"use (region list mirrors eks-agent-platform/docs/runbooks/import-open-weight-model.md; update the runbook "+
				"first)", c.Cloud.Region, strings.Join(bedrockCustomModelImportRegions, ", ")))
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
