package phases

import (
	"slices"
	"testing"

	"github.com/rackctl/rackctl/internal/config"
)

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
	cfg.Addons.Observability = true
	comps := CoreComponents(cfg)

	mm, cb := indexOf(comps, "managed-monitoring"), indexOf(comps, "cluster-bootstrap")
	if mm < 0 {
		t.Fatalf("managed-monitoring missing when addons.observability is set; got %v", comps)
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

func TestCoreComponents_NetworkFirstAddonsLast(t *testing.T) {
	cfg := baseCfg()
	cfg.Addons.Observability = true
	cfg.DNS = &config.DNS{HostedZone: "example.com"}
	comps := CoreComponents(cfg)

	if comps[0] != "network" {
		t.Errorf("network must be applied first; got %v", comps)
	}
	if comps[len(comps)-1] != "cluster-addons" {
		t.Errorf("cluster-addons must be applied last; got %v", comps)
	}
	// The cluster must exist before anything that talks to its API.
	if indexOf(comps, "cluster") > indexOf(comps, "cluster-bootstrap") {
		t.Errorf("cluster must precede cluster-bootstrap; got %v", comps)
	}
}

// bootstrapComponents must be a faithful SUBSEQUENCE of CoreComponents. This is the
// invariant that broke: CoreComponents was only read by destroy, while the apply
// path walked its own hardcoded {"secrets","cluster-bootstrap"} — so the two
// disagreed and agent-iam / managed-monitoring / dns were never applied at all.
func TestBootstrapComponents_IsSubsequenceOfCore(t *testing.T) {
	for name, cfg := range map[string]*config.Config{
		"defaults": baseCfg(),
		"all-on": func() *config.Config {
			c := baseCfg()
			c.Addons.Observability = true
			c.DNS = &config.DNS{HostedZone: "example.com"}
			return c
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			core := CoreComponents(cfg)
			boot := bootstrapComponents(cfg)

			// every bootstrap component appears in core, in the same relative order
			prev := -1
			for _, c := range boot {
				i := indexOf(core, c)
				if i < 0 {
					t.Fatalf("bootstrapComponents has %q which is not in CoreComponents %v", c, core)
				}
				if i <= prev {
					t.Fatalf("bootstrapComponents order diverges from CoreComponents: %v vs %v", boot, core)
				}
				prev = i
			}
			// it owns everything except what the cluster and addons phases apply
			for _, c := range core {
				owned := c != "network" && c != "cluster" && c != "cluster-addons"
				if owned && indexOf(boot, c) < 0 {
					t.Errorf("component %q is in CoreComponents but no phase applies it; got %v", c, boot)
				}
			}
		})
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
