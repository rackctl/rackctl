package cmd

import (
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/phases"
)

// A component the config turned off after applying it is still standing. The destroy reads
// its state, tears it down with the rest and names it; the config alone used to skip it and
// print "platform destroyed" over a druid cluster still billing. Each of these runs as a
// dry-run, which reads state for real and so takes the decision the real destroy would.
func TestDestroy_TearsDownAComponentTheConfigTurnedOff(t *testing.T) {
	out, err := runDestroy(t, `case "$*" in
  *development/druid/terraform.tfstate*) echo '{"resources":[{"type":"aws_rds_cluster"}]}' ;;
esac
exit 0
`, "--dry-run")
	if code := exitStatus(err); code != 0 {
		t.Fatalf("want exit 0, got %d\n%s", code, out)
	}
	for _, want := range []string{"druid is not in this config but its state holds 1 resource(s)", "destroy druid",
		"platform destroyed"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

// State that could not be read for a component the config does not apply may be standing,
// so the destroy names it and exits resources-may-remain rather than 0.
func TestDestroy_UnreadStateForAComponentTheConfigSkipsIsNotClean(t *testing.T) {
	out, err := runDestroy(t, `case "$*" in
  *development/druid/terraform.tfstate*) echo 'An error occurred (AccessDenied)' >&2; exit 254 ;;
esac
exit 0
`, "--dry-run")
	if code := exitStatus(err); code != ExitStranded {
		t.Fatalf("want exit %d, got %d\n%s", ExitStranded, code, out)
	}
	if !strings.Contains(out, "the state of druid, which this config does not apply, could not be read") ||
		strings.Contains(out, "platform destroyed") {
		t.Errorf("the unread component must be named and the teardown must not claim success:\n%s", out)
	}
}

// The account-scoped roots stay behind --account-scoped, and a destroy that leaves them
// standing names them and does not exit 0.
func TestDestroy_AccountScopedRootsLeftStandingAreNamed(t *testing.T) {
	out, err := runDestroy(t, `case "$1 $2" in
  "s3api list-objects-v2") echo 'org/bedrock-account/terraform.tfstate' ; exit 0 ;;
esac
case "$*" in
  *org/bedrock-account/terraform.tfstate*) echo '{"resources":[{"type":"aws_s3_bucket"}]}' ;;
esac
exit 0
`, "--dry-run")
	if code := exitStatus(err); code != ExitStranded {
		t.Fatalf("want exit %d, got %d\n%s", ExitStranded, code, out)
	}
	for _, want := range []string{"the account-scoped roots bedrock-account (1 resource(s)) still hold state",
		"--account-scoped --force-buckets", "cannot vouch for everything"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "platform destroyed") {
		t.Errorf("\"platform destroyed\" over account roots left standing:\n%s", out)
	}
}

// The agent-platform tree is read from state when the config turns the platform off, and a
// root that still holds resources is destroyed. Before, a disabled platform skipped the tree.
func TestDestroy_TearsDownAnAgentPlatformTheConfigTurnedOff(t *testing.T) {
	out, err := runDestroyDryRun(t, `case "$1 $2" in
  "s3api list-objects-v2") echo 'development-platform/bedrock/terraform.tfstate' ; exit 0 ;;
esac
case "$*" in
  *development-platform/bedrock/terraform.tfstate*) echo '{"resources":[{"type":"aws_iam_role"}]}' ;;
esac
exit 0
`)
	if code := exitStatus(err); code != 0 {
		t.Fatalf("want exit 0, got %d\n%s", code, out)
	}
	for _, want := range []string{"bedrock is not in this config but its state holds 1 resource(s)",
		"--working-dir terraform/live/development-platform/bedrock --non-interactive destroy"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

// A dns component torn down from state takes the zone its state records, so its destroy has
// the domain it was built for rather than refusing for want of dns.hostedZone.
func TestDestroy_ADNSComponentFromStateTakesTheZoneItsStateRecords(t *testing.T) {
	out, err := runDestroyDryRun(t, `case "$*" in
  *development/dns/terraform.tfstate*) echo '{"resources":[{"type":"aws_route53_zone","instances":[{"attributes":{"name":"acme.example.com."}}]}]}' ;;
esac
exit 0
`)
	if code := exitStatus(err); code != 0 {
		t.Fatalf("want exit 0, got %d\n%s", code, out)
	}
	if !strings.Contains(out, "TF_VAR_domain_name=acme.example.com") {
		t.Errorf("the dns destroy must carry the zone its state records:\n%s", out)
	}
}

// --force-buckets permits a bucket-owning component the destroy reaches from state.
func TestDestroy_ForceBucketsPermitsAComponentFromState(t *testing.T) {
	out, err := runDestroy(t, `case "$*" in
  *development/druid/terraform.tfstate*) echo '{"resources":[{"type":"aws_s3_bucket"}]}' ;;
esac
exit 0
`, "--dry-run", "--force-buckets")
	if code := exitStatus(err); code != 0 {
		t.Fatalf("want exit 0, got %d\n%s", code, out)
	}
	if !strings.Contains(out, "apply druid with TF_VAR_force_destroy_buckets=true") {
		t.Errorf("druid, reached from state, must be permitted before its destroy:\n%s", out)
	}
}

// teardownRemains is what keeps a destroy that could not read a component's state from
// exiting 0.
func TestTeardownRemains_NamesUnreadComponents(t *testing.T) {
	if got := teardownRemains(phases.TeardownSet{}); got != nil {
		t.Errorf("nothing unread leaves nothing to report, got %v", got)
	}
	got := teardownRemains(phases.TeardownSet{Unread: []string{"druid", "dns"}})
	if len(got) != 1 || !strings.Contains(got[0].Error(), "the state of druid, dns") {
		t.Errorf("want one error naming both components, got %v", got)
	}
}
