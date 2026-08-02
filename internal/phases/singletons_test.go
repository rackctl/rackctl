package phases

import (
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/config"
)

// accountSingletons are the landing-zone components that exist ONCE per account, not once per
// environment. They provision org-wide identity, org-wide guardrails, the account's service
// quotas and its break-glass access.
//
// Under rackctl every environment shares one account (there is no provider assume_role seam
// upstream, so which account a leaf lands in is decided entirely by ambient credentials). That
// makes applying any of these from a workload run a cross-environment write by construction:
// two environments would fight over one org-scoped resource, and a teardown of either would
// destroy it for both. break-glass is the sharpest — a teardown that removes the account's
// emergency access path removes the thing you would use to fix the teardown.
var accountSingletons = []string{
	"org-cost", "org-compliance", "org-scp", "org-networking", "org-identity",
	"github-oidc", "service-quotas", "break-glass",
}

// rackctl must never apply one.
//
// This is asserted as a TEST rather than as a runtime guard, and the distinction is deliberate.
// CoreComponents and agentPlatformComponents are fixed slices — no config field can inject a
// component name, and componentDir only ever composes a path from one of them — so a runtime
// check could not fire and would be exactly the vestigial scaffold the greenfield doctrine
// forbids. What CAN happen is somebody adding one to the slice, and that is a change to this
// repo, which is what a test catches.
//
// The permutation sweep matters: several components are conditional, so a singleton appended
// inside one of those branches would be invisible to a single default-config assertion.
func TestComponents_NeverApplyAnAccountSingleton(t *testing.T) {
	bools := []bool{false, true}
	for _, agent := range bools {
		for _, modelImport := range bools {
			for _, druid := range bools {
				for _, dns := range bools {
					for _, envName := range []config.Environment{config.EnvDev, config.EnvStaging, config.EnvProduction} {
						cfg := config.Default()
						cfg.Environment = envName
						cfg.Cluster.Name = "platform"
						cfg.AgentPlatform.Enable = &agent
						cfg.AgentPlatform.ModelImport = modelImport
						cfg.Addons.Druid = druid
						if dns {
							cfg.DNS = &config.DNS{HostedZone: "example.test"}
						} else {
							cfg.DNS = nil
						}

						got := append(CoreComponents(cfg), agentPlatformComponents()...)
						for _, c := range got {
							for _, s := range accountSingletons {
								if c == s || (strings.HasPrefix(s, "org-") && strings.HasPrefix(c, "org-")) {
									t.Fatalf("component %q is an account singleton and must never be applied "+
										"by a workload run — every environment shares one account, so applying "+
										"it is a cross-environment write and destroying it removes the resource "+
										"for every other environment too.\nconfig: env=%s agent=%v modelImport=%v "+
										"druid=%v dns=%v\ncomponents: %v",
										c, envName, agent, modelImport, druid, dns, got)
								}
							}
						}
					}
				}
			}
		}
	}
}

// The companion property, and the reason the one above is not vacuous: componentDir can only
// ever address this environment's own workload tree. If that stops being true, the assertion
// above stops being sufficient on its own.
func TestComponentDir_StaysInsideTheWorkloadTree(t *testing.T) {
	cfg := config.Default()
	cfg.Environment = config.EnvStaging
	cfg.Cluster.Name = "platform"
	st := testState(cfg)

	for _, c := range CoreComponents(cfg) {
		dir := componentDir(st, c)
		want := "live/aws/workload-staging/"
		if !strings.HasPrefix(dir, want) {
			t.Fatalf("componentDir(%q) = %q, which is outside %q — a component addressed outside "+
				"the workload tree is one that can reach the management or network account",
				c, dir, want)
		}
	}
}
