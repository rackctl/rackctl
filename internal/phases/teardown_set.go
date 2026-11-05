package phases

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/engine"
)

// TeardownSet is the landing-zone walk a standalone destroy takes, decided by what state
// exists rather than by what today's config asks for.
//
// CoreComponents is a function of the config, and nothing records what an earlier apply
// built. So a config that turned druid, dns, the portal or model-import off after applying
// them walked a set without them: their state was never visited, their resources kept
// billing, and the destroy printed "platform destroyed". The config says what to BUILD; what
// to tear down is what is standing.
type TeardownSet struct {
	// Components is the walk, in apply order: every component this config applies, and
	// every one it does not whose state still holds resources.
	Components []string
	// Unconfigured names the components in Components this config does not apply.
	Unconfigured []string
	// Unread names the components this config does not apply whose state could not be
	// read. Each may be standing; none is walked, because nothing proves there is anything
	// to destroy, and a destroy that cannot vouch for them does not exit 0.
	Unread []string
	// Zone is the hosted zone the dns state records, when dns is torn down from state and
	// the config names no dns.hostedZone. The dns component and the delegation withdrawal
	// both need it.
	Zone string
}

// allCoreComponents is every landing-zone component rackctl can apply, in apply order: the
// list CoreComponents returns for a config that turns every gate on. Derived rather than
// restated, so a component added there is one a destroy probes for.
func allCoreComponents(cfg *config.Config) []string {
	all := *cfg
	on := true
	all.AgentPlatform.Enable = &on
	all.AgentPlatform.ModelImport = true
	all.Observability.Tier = config.TierFull
	all.Addons.Druid = true
	all.DNS = &config.DNS{HostedZone: "every-component"}
	all.ControlPlane.EKSFleet = true
	all.ControlPlane.Portal = true
	return CoreComponents(&all)
}

// landingZoneStateKey is a component's state object in landing-zone's backend bucket. The
// backend derives the key from the leaf's path under live/, prefixed with the environment.
func landingZoneStateKey(st *engine.State, component string) string {
	return string(st.Config.Environment) + "/" + strings.TrimPrefix(componentDir(st, component), "live/") +
		"/terraform.tfstate"
}

// landingZoneStateBucket is landing-zone's backend bucket for this account and region.
func landingZoneStateBucket(st *engine.State) string {
	return fmt.Sprintf("%s-%s-tfstate", st.Config.Cloud.AccountID, st.Config.Cloud.Region)
}

// lzState is the part of a terraform state object this file reads.
type lzState struct {
	Resources []struct {
		Type      string `json:"type"`
		Instances []struct {
			Attributes struct {
				Name string `json:"name"`
			} `json:"attributes"`
		} `json:"instances"`
	} `json:"resources"`
}

// readLandingZoneState reads one component's state straight from S3. Not through terragrunt:
// `terragrunt state list` reinitialises the backend, which a read that runs under --dry-run
// has no business doing. An object S3 says is absent, and an empty one, are no state.
func readLandingZoneState(ctx context.Context, st *engine.State, component string) (lzState, error) {
	var s lzState
	bucket := landingZoneStateBucket(st)
	key := landingZoneStateKey(st, component)
	raw, err := st.Runner.Query(ctx, "aws", "s3", "cp", "s3://"+bucket+"/"+key, "-",
		"--region", st.Config.Cloud.Region)
	if err != nil {
		if s3BucketAbsent(err) || strings.Contains(err.Error(), "NoSuchKey") {
			return s, nil
		}
		return s, fmt.Errorf("aws s3 cp s3://%s/%s: %w", bucket, key, err)
	}
	if strings.TrimSpace(raw) == "" {
		return s, nil
	}
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return s, fmt.Errorf("s3://%s/%s is not readable terraform state: %w", bucket, key, err)
	}
	return s, nil
}

// zoneName is the hosted zone a dns state records, without its trailing dot.
func (s lzState) zoneName() string {
	for _, r := range s.Resources {
		if r.Type != "aws_route53_zone" {
			continue
		}
		for _, i := range r.Instances {
			if n := strings.TrimSuffix(i.Attributes.Name, "."); n != "" {
				return n
			}
		}
	}
	return ""
}

// TeardownComponents decides the landing-zone walk for a standalone destroy. Every component
// this config applies is walked, as before. Every other one is read from state, read-only and
// under --dry-run too, and walked when it still holds resources, each named so the operator
// sees what the config no longer mentions.
func TeardownComponents(ctx context.Context, st *engine.State) TeardownSet {
	var set TeardownSet
	configured := map[string]bool{}
	for _, c := range CoreComponents(st.Config) {
		configured[c] = true
	}
	for _, c := range allCoreComponents(st.Config) {
		if configured[c] {
			set.Components = append(set.Components, c)
			continue
		}
		s, err := readLandingZoneState(ctx, st, c)
		switch {
		case err != nil:
			set.Unread = append(set.Unread, c)
			note(st, "destroy: %s is not in this config, and whether it still holds state could not be read "+
				"(%v) — it is not walked, and the destroy will not report a clean teardown", c, err)
			continue
		case len(s.Resources) == 0:
			continue
		}
		set.Components = append(set.Components, c)
		set.Unconfigured = append(set.Unconfigured, c)
		note(st, "destroy: %s is not in this config but its state holds %d resource(s) — tearing it down "+
			"with the rest, because a component turned off after an apply is still standing", c, len(s.Resources))
		if c == "dns" && !st.Config.HasDNS() {
			set.Zone = s.zoneName()
		}
	}
	return set
}
