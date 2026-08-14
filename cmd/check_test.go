package cmd

import (
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/config"
)

// The kubeconfig entry EKS writes is the cluster ARN, so comparing it directly
// against cfg.ClusterName() never matches and the health set would never run.
// Anything that is NOT an EKS ARN must come back unchanged, so that a kind or
// minikube context is reported as itself and refused, rather than being parsed
// into something that coincidentally equals the configured name.
func TestEKSClusterName(t *testing.T) {
	for _, tc := range []struct{ name, entry, want string }{
		{"eks arn", "arn:aws:eks:us-west-2:351619759866:cluster/development-platform", "development-platform"},
		{"gov partition", "arn:aws-us-gov:eks:us-gov-west-1:111111111111:cluster/prod-platform", "prod-platform"},
		{"kind context", "kind-rackctl", "kind-rackctl"},
		{"bare name", "development-platform", "development-platform"},
		{"empty", "", ""},
		{"name containing the separator", "arn:aws:eks:us-west-2:1:cluster/a:cluster/b", "b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := eksClusterName(tc.entry); got != tc.want {
				t.Errorf("eksClusterName(%q) = %q, want %q", tc.entry, got, tc.want)
			}
		})
	}
}

// Identity is composed once so two runners in the same command cannot disagree.
// `rackctl check` pinned the profile on its preflight runner and not on the one it
// handed to doctor, so the two halves of a single command asked AWS as different
// identities.
func TestAWSEnvPinsProfileAndRegion(t *testing.T) {
	cfg := &config.Config{}
	cfg.Cloud.Profile = "stxkxs"
	cfg.Cloud.Region = "us-west-2"

	got := awsEnv(cfg)
	want := map[string]bool{"AWS_PROFILE=stxkxs": true, "AWS_REGION=us-west-2": true}
	if len(got) != len(want) {
		t.Fatalf("awsEnv = %v, want exactly %d entries", got, len(want))
	}
	for _, e := range got {
		if !want[e] {
			t.Errorf("unexpected entry %q", e)
		}
	}
}

// tgEnv must carry the same identity, not a second hand-written copy of it. It
// builds on awsEnv via append, and append can alias — assert the terragrunt
// environment does not come back missing the AWS pair.
func TestTGEnvIncludesAWSIdentity(t *testing.T) {
	cfg := &config.Config{}
	cfg.Cloud.Profile = "stxkxs"
	cfg.Cloud.Region = "us-west-2"
	cfg.Cloud.AccountID = "351619759866"

	env := tgEnv(cfg)
	for _, want := range []string{"AWS_PROFILE=stxkxs", "AWS_REGION=us-west-2", "TERRAGRUNT_ACCOUNT_ID=351619759866"} {
		var found bool
		for _, e := range env {
			if e == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("tgEnv missing %q\ngot: %v", want, env)
		}
	}
}

// awsEnv must hand back a fresh slice each call. tgEnv appends to it, and a shared
// backing array would let one caller's terragrunt variables leak into another's.
func TestAWSEnvDoesNotShareBackingArray(t *testing.T) {
	cfg := &config.Config{}
	cfg.Cloud.Profile = "p"
	cfg.Cloud.Region = "r"

	a := awsEnv(cfg)
	_ = append(a, "TF_VAR_leaked=yes") //nolint:gocritic // deliberately exercising aliasing
	b := awsEnv(cfg)

	for _, e := range b {
		if e == "TF_VAR_leaked=yes" {
			t.Fatal("awsEnv returned a slice sharing state with a previous call")
		}
	}
}

// The destroy runbook tells operators to "confirm the account, region, and profile
// in the printed title before you run it". The title carried the org name, the
// region and the environment — neither of the two fields that decide WHICH CLOUD is
// about to be changed. A region is shared by every account an operator has, so the
// instruction was unfollowable as written.
func TestCommandTitleCarriesAccountAndProfile(t *testing.T) {
	cfg := &config.Config{}
	cfg.Org.Name = "acme"
	cfg.Cloud.AccountID = "351619759866"
	cfg.Cloud.Profile = "stxkxs"
	cfg.Cloud.Region = "us-west-2"
	cfg.Environment = "development"

	got := commandTitle("destroy", cfg)

	for _, want := range []string{"destroy", "acme", "351619759866", "stxkxs", "us-west-2", "development"} {
		if !strings.Contains(got, want) {
			t.Errorf("title %q is missing %q", got, want)
		}
	}
}

// One helper for all three commands, so the banner cannot say different things
// depending on which verb you reached it by.
func TestCommandTitleIsConsistentAcrossVerbs(t *testing.T) {
	cfg := &config.Config{}
	cfg.Org.Name = "acme"
	cfg.Cloud.AccountID = "351619759866"
	cfg.Cloud.Profile = "stxkxs"
	cfg.Cloud.Region = "us-west-2"
	cfg.Environment = "development"

	base := strings.TrimPrefix(commandTitle("apply", cfg), "rackctl apply")
	for _, verb := range []string{"plan", "destroy", "check"} {
		if got := strings.TrimPrefix(commandTitle(verb, cfg), "rackctl "+verb); got != base {
			t.Errorf("%s title tail = %q, want %q — the verbs have drifted apart", verb, got, base)
		}
	}
}
