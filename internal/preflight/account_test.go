package preflight

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/doctor"
)

// ─────────────────────────── bucket names ───────────────────────────
//
// The package header opens with the failure it exists for — "BucketAlreadyExists on a bucket
// name that is globally unique across every AWS account on earth. Unrecoverable by retry.
// Discovered 6 minutes in." — and nothing checked a bucket name until now. These pin the three
// outcomes apart, because collapsing them wastes the check: one is recoverable by a destroy,
// one is recoverable only by renaming the cluster, and one is not a problem at all.

// A state backend that already exists is the STEADY STATE, not wreckage. Getting this wrong
// made the check fail against the account it was written for, and it would have failed on every
// account that had ever run an install — a preflight that fires on health teaches operators to
// skip it.
func TestBucketNames_StateBackendExistingIsNotACollision(t *testing.T) {
	fakeBin(t, "aws", `
case "$1 $2" in
  "s3api list-buckets") echo "111122223333-us-west-2-tfstate" ;;
  "eks describe-cluster") exit 1 ;;      # no live cluster => not a re-apply
  "s3api head-bucket")    exit 1 ;;      # every other name is free
  *) exit 1 ;;
esac`)
	r := CheckBucketNames(context.Background(), testEnv())
	if r.Status != doctor.OK {
		t.Fatalf("the terraform state bucket is created idempotently, shared across environments "+
			"and never deleted by rackctl — its existence is normal.\ngot %s: %s", r.Status, r.Detail)
	}
}

// A COMPONENT bucket that already exists, with no live cluster, is wreckage: its component's
// state and the account have diverged, and the next apply fails on it.
func TestBucketNames_ComponentBucketLeftBehindFails(t *testing.T) {
	fakeBin(t, "aws", `
case "$1 $2" in
  "s3api list-buckets") echo "development-platform-111122223333-us-west-2-velero" ;;
  "eks describe-cluster") exit 1 ;;
  "s3api head-bucket")    exit 1 ;;
  *) exit 1 ;;
esac`)
	r := CheckBucketNames(context.Background(), testEnv())
	mustFail(t, r, "rackctl destroy")
	if !strings.Contains(r.Detail, "velero") {
		t.Errorf("the failure must name the bucket and its component:\n%s", r.Detail)
	}
}

// The same bucket, with the cluster LIVE, is a re-apply and its own infrastructure.
func TestBucketNames_ReapplyIsNotACollision(t *testing.T) {
	fakeBin(t, "aws", `
case "$1 $2" in
  "s3api list-buckets") echo "development-platform-111122223333-us-west-2-velero" ;;
  "eks describe-cluster") echo ACTIVE ;;
  "s3api head-bucket")    exit 1 ;;
  *) exit 1 ;;
esac`)
	if r := CheckBucketNames(context.Background(), testEnv()); r.Status != doctor.OK {
		t.Fatalf("a live cluster makes its own buckets its own, not collisions.\ngot %s: %s", r.Status, r.Detail)
	}
}

// An ACCOUNT-SCOPED bucket that already exists must not fail the install.
//
// eks-agent-platform's live/org roots own one set of these per account and region, shared by
// every environment. Whichever environment installs first creates them, so from the second
// environment onwards finding them is the healthy steady state — and failing here would refuse
// every install after the first, which is the same shape as the state-backend false positive
// this scope split generalises.
//
// It is still reported, because on a first install in a fresh account the identical observation
// means wreckage and rackctl cannot tell the two apart.
func TestBucketNames_AccountScopedBucketIsReportedNotRefused(t *testing.T) {
	fakeBin(t, "aws", `
case "$1 $2" in
  "s3api list-buckets") echo "org-111122223333-us-west-2-bedrock-invocations" ;;
  "eks describe-cluster") exit 1 ;;      # this environment's cluster does not exist yet
  "s3api head-bucket")    exit 1 ;;
  *) exit 1 ;;
esac`)
	r := CheckBucketNames(context.Background(), testEnv())
	if r.Status != doctor.Warn {
		t.Fatalf("an account+region singleton another environment already created is not this "+
			"install's collision — failing here refuses every install after the first.\n"+
			"got %s: %s", r.Status, r.Detail)
	}
	if !strings.Contains(r.Detail, "org-111122223333-us-west-2-bedrock-invocations") {
		t.Errorf("the warning must name what it found, so a first-install operator can tell "+
			"wreckage from the steady state:\n%s", r.Detail)
	}
}

// And the account-scoped names must be the ones upstream actually creates. Composing them from
// cfg.Environment would produce development-…-bedrock-invocations, which nothing creates: the
// check would look for names that cannot exist while missing the ones that do, and report a
// clean preflight over an unchecked estate.
func TestPlannedBuckets_AccountScopedNamesCarryTheOrgToken(t *testing.T) {
	cfg := testEnv().Cfg
	var got []string
	for _, b := range plannedBuckets(cfg) {
		got = append(got, b.name)
	}
	for _, want := range []string{
		"org-111122223333-us-west-2-bedrock-invocations",
		"org-111122223333-us-west-2-bedrock-access-logs",
		"org-111122223333-us-west-2-cost-athena-111122223333",
		"org-111122223333-us-west-2-cost-estimates-111122223333",
		"org-111122223333-us-west-2-cost-access-logs-111122223333",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %q — this is the name terraform/live/org creates, with `org` pinned "+
				"by that tree's env.hcl rather than derived from the config.\ngot: %v", want, got)
		}
	}
	// The pre-move names are gone, not merely joined by the new ones. Keeping both would make
	// every install report a phantom collision the moment one of the old orphans is present.
	for _, gone := range []string{
		"development-platform-bedrock-invocations-111122223333",
		"development-platform-cost-cur-111122223333",
	} {
		if slices.Contains(got, gone) {
			t.Errorf("%q is the pre-account-scoping shape and nothing creates it now", gone)
		}
	}
}

// The unrecoverable one, and it must not read like the other two. S3's namespace is global: a
// name owned by another account is gone, and no cleanup here frees it.
func TestBucketNames_OwnedByAnotherAccountIsUnrecoverable(t *testing.T) {
	fakeBin(t, "aws", `
case "$1 $2" in
  "s3api list-buckets") echo "" ;;       # we own none of them
  "eks describe-cluster") exit 1 ;;
  "s3api head-bucket")    exit 0 ;;      # ...but they all exist. 403-shaped: reachable, not ours
  *) exit 1 ;;
esac`)
	r := CheckBucketNames(context.Background(), testEnv())
	mustFail(t, r, "cluster.name")
	if !strings.Contains(r.Detail, "NOT owned by this account") {
		t.Errorf("this outcome is unrecoverable by cleanup and must say so, or an operator will "+
			"spend a teardown trying:\n%s", r.Detail)
	}
}

// config.Validate caps cluster.name at 12 chars so the derived names fit — but its own message
// says "in us-west-2", and the cap is not region-aware. A 14-character region breaches 63 with a
// config that validates cleanly, and the apply then fails with a message about S3 naming rules
// that never mentions the knob.
func TestBucketNames_LengthIsRegionDependent(t *testing.T) {
	fakeBin(t, "aws", `exit 1`)
	env := testEnv()
	env.Cfg.Environment = config.EnvProduction
	env.Cfg.Cluster.Name = "analyticsxy" // 11 chars: inside config.Validate's cap
	env.Cfg.Cloud.Region = "ap-southeast-4"

	r := CheckBucketNames(context.Background(), env)
	mustFail(t, r, "63-character")
	if !strings.Contains(r.Detail, "model-artifacts") {
		t.Errorf("agent-iam's model-artifacts bucket is the longest name and the binding "+
			"constraint; the failure must name it:\n%s", r.Detail)
	}
}

// ─────────────────────────── bedrock singleton ───────────────────────────

// The Bedrock API holds exactly one invocation-logging configuration per account per region, and
// this account is shared with other estates — so a configuration pointing anywhere other than
// the account root's own bucket belongs to something else, and applying here takes it over.
func TestBedrockLogging_SomebodyElsesSingletonRefuses(t *testing.T) {
	fakeBin(t, "aws", `
case "$2" in
  get-model-invocation-logging-configuration) echo "some-other-estate-bedrock-logs" ;;
  *) exit 1 ;;
esac`)
	r := CheckBedrockLogging(context.Background(), testEnv())
	mustFail(t, r, "ONE such configuration per")
	if !strings.Contains(r.Detail, "some-other-estate-bedrock-logs") {
		t.Errorf("the failure must name the CURRENT owner, or the operator cannot tell whose "+
			"logging they are about to take:\n%s", r.Detail)
	}
}

// The account-scoped singleton is a no-op for EVERY environment, not just the one that created
// it — which is the whole point of the upstream move to live/org/bedrock-account.
//
// This test previously fed it "development-platform-bedrock-invocations-<acct>", the pre-move
// cluster-scoped shape. That name matched what the check composed, so it passed — while the
// name nothing produces drifted out from under it. The comment said "without this the check
// would refuse every re-apply", and that is precisely what the check had started doing against
// a correctly built account; the test could not see it because both sides were stale together.
//
// It is now fed the name bedrock-account actually creates
// (components/bedrock-account/main.tf:11,152 — prefix "${var.environment}-${account}-${region}
// -bedrock" with var.environment pinned to "org" by live/org/env.hcl), so the constant and the
// fixture can no longer drift as a pair.
func TestBedrockLogging_TheAccountScopedSingletonIsFineFromAnyEnvironment(t *testing.T) {
	fakeBin(t, "aws", `
case "$2" in
  get-model-invocation-logging-configuration) echo "org-111122223333-us-west-2-bedrock-invocations" ;;
  *) exit 1 ;;
esac`)
	// testEnv() is the development environment; the singleton carries no environment token, and
	// that is what makes this pass rather than being a takeover.
	if r := CheckBedrockLogging(context.Background(), testEnv()); r.Status != doctor.OK {
		t.Fatalf("the account-scoped singleton belongs to every environment in the account, so "+
			"re-applying against it must pass.\ngot %s: %s", r.Status, r.Detail)
	}
}

// The pre-move name must NOT be treated as ours. It is the shape a 2026-05 run left behind in
// this very account, and adopting it would mean pointing the account's invocation logging at an
// orphan bucket that nothing owns and no state describes.
func TestBedrockLogging_ThePreMoveClusterScopedNameIsNotOurs(t *testing.T) {
	fakeBin(t, "aws", `
case "$2" in
  get-model-invocation-logging-configuration) echo "development-platform-bedrock-invocations-111122223333" ;;
  *) exit 1 ;;
esac`)
	r := CheckBedrockLogging(context.Background(), testEnv())
	mustFail(t, r, "ONE such configuration per")
}

// ─────────────────────────── hosted zone ───────────────────────────

// dns runs in create mode, so an apply MINTS a zone and a destroy DELETES one. A second public
// zone for a delegated domain is silently useless in both directions.
func TestHostedZone_ExistingZoneRefuses(t *testing.T) {
	fakeBin(t, "aws", `
case "$1 $2" in
  "route53 list-hosted-zones") printf 'nanohype.dev.\t/hostedzone/Z123\tFalse\n' ;;
  *) exit 1 ;;
esac`)
	env := testEnv()
	env.Cfg.DNS = &config.DNS{HostedZone: "nanohype.dev"}
	r := CheckHostedZone(context.Background(), env)
	mustFail(t, r, "create mode")
}

// A trailing dot in the config must not make the same zone look absent.
func TestHostedZone_MatchesRegardlessOfTrailingDot(t *testing.T) {
	fakeBin(t, "aws", `
case "$1 $2" in
  "route53 list-hosted-zones") printf 'nanohype.dev.\t/hostedzone/Z123\tFalse\n' ;;
  *) exit 1 ;;
esac`)
	env := testEnv()
	env.Cfg.DNS = &config.DNS{HostedZone: "nanohype.dev."}
	mustFail(t, CheckHostedZone(context.Background(), env), "already has")
}

// ─────────────────────────── session lifetime ───────────────────────────

// A run that dies half-applied because the token lapsed mid-phase cannot even roll itself back,
// because the rollback needs the same credentials.
func TestSessionLifetime_ShortSessionRefusesBeforeSpending(t *testing.T) {
	fakeBin(t, "aws", `echo '{"Version":1,"Expiration":"2020-01-01T00:00:00+00:00"}'`)
	mustFail(t, CheckSessionLifetime(context.Background(), testEnv()), "aws sso login")
}

// Credentials with no expiry (static keys, an instance role) are not a short session. Failing
// on "I could not read an expiry" would block every non-SSO caller.
func TestSessionLifetime_NoExpiryIsNotAFailure(t *testing.T) {
	fakeBin(t, "aws", `echo '{"Version":1,"AccessKeyId":"AKIA"}'`)
	if r := CheckSessionLifetime(context.Background(), testEnv()); r.Status != doctor.OK {
		t.Fatalf("no expiry means nothing to expire.\ngot %s: %s", r.Status, r.Detail)
	}
}

// ─────────────────────────── cost allocation ───────────────────────────
//
// Two halves of one bill, attributed by different mechanisms, activated separately. The check
// used to look only at the bare key — which reports healthy on an account whose model spend,
// the dominant cost, is entirely unattributed.

// Both keys active covers the tenant's DATASTORES and says nothing about model spend. A
// Bedrock invocation is not a taggable resource, so no resourceTags/ key is ever populated on
// one — this must not read as clear.
func TestCostAllocation_ResourceTagsAloneDoNotCoverModelSpend(t *testing.T) {
	fakeBin(t, "aws", `echo "PlatformId	Repository"`)
	r := CheckCostAllocationTags(context.Background(), testEnv())
	if r.Status == doctor.OK {
		t.Fatalf("PlatformId and Repository being active covers datastores only. Model spend is "+
			"attributed by calling identity, activated separately, and is the dominant cost — "+
			"reporting OK here is the exact false clear this check exists to stop.\ngot: %s", r.Detail)
	}
	if !strings.Contains(r.Detail, "iamPrincipal/PlatformId") {
		t.Errorf("the warning must name the key that is missing:\n%s", r.Detail)
	}
	// And it must not send the operator to activate it right now — for Bedrock the key does not
	// appear for activation until a tagged principal has made at least one call.
	if !strings.Contains(r.Detail, "at least one call") {
		t.Errorf("the remedy must say this one comes AFTER traffic, or the operator goes looking "+
			"for a key that cannot exist yet:\n%s", r.Detail)
	}
}

// With the IAM-principal half active too, both halves are covered and the check is clear.
func TestCostAllocation_BothHalvesActiveIsClear(t *testing.T) {
	fakeBin(t, "aws", `echo "PlatformId	Repository	iamPrincipal/PlatformId"`)
	if r := CheckCostAllocationTags(context.Background(), testEnv()); r.Status != doctor.OK {
		t.Fatalf("both attribution mechanisms are active; nothing is missing.\ngot %s: %s", r.Status, r.Detail)
	}
}

// Neither half active must report BOTH, not just the first one found. They are activated
// through different console filters, so an operator who fixes only what they were told about
// fixes half the bill.
func TestCostAllocation_NothingActiveReportsBothHalves(t *testing.T) {
	fakeBin(t, "aws", `echo ""`)
	r := CheckCostAllocationTags(context.Background(), testEnv())
	if r.Status != doctor.Warn {
		t.Fatalf("missing cost allocation tags truncate a bill rather than blocking an install, "+
			"so this warns.\ngot %s: %s", r.Status, r.Detail)
	}
	for _, want := range []string{"PlatformId", "Repository", "iamPrincipal/PlatformId", "SEPARATELY"} {
		if !strings.Contains(r.Detail, want) {
			t.Errorf("the warning must name %q — the two halves are activated separately, so "+
				"reporting one leaves the other silently broken:\n%s", want, r.Detail)
		}
	}
}

// A zone this install's own dns state already tracks is not a collision. Re-applying it is a
// no-op, and reading it as a fault makes `rackctl apply` refuse to run a second time against
// a platform it built itself — which is exactly the situation after any later phase fails and
// the operator wants to resume.
func TestHostedZone_AZoneOurStateOwnsIsARepply(t *testing.T) {
	fakeBin(t, "aws", `
case "$1 $2" in
  "route53 list-hosted-zones") printf 'hub.nanohype.dev.\t/hostedzone/Z999\tFalse\n' ;;
  "s3 cp") echo '{"resources":[{"type":"aws_route53_zone","instances":[{"attributes":{"zone_id":"Z999"}}]}]}' ;;
  *) exit 1 ;;
esac`)
	env := testEnv()
	env.Cfg.DNS = &config.DNS{HostedZone: "hub.nanohype.dev"}

	r := CheckHostedZone(context.Background(), env)
	if r.Status != doctor.OK {
		t.Fatalf("a zone in our own dns state is a re-apply, not a second zone: %s — %s", r.Status, r.Detail)
	}
}

// And a zone we do NOT own still refuses, which is the whole point of the check. The state
// read must narrow the failure, never remove it.
func TestHostedZone_AZoneWeDoNotOwnStillRefuses(t *testing.T) {
	fakeBin(t, "aws", `
case "$1 $2" in
  "route53 list-hosted-zones") printf 'hub.nanohype.dev.\t/hostedzone/ZSOMEONEELSE\tFalse\n' ;;
  "s3 cp") echo '{"resources":[{"type":"aws_route53_zone","instances":[{"attributes":{"zone_id":"Z999"}}]}]}' ;;
  *) exit 1 ;;
esac`)
	env := testEnv()
	env.Cfg.DNS = &config.DNS{HostedZone: "hub.nanohype.dev"}

	mustFail(t, CheckHostedZone(context.Background(), env), "create mode")
}

// Unreadable state must not read as ownership. Failing to prove we own a zone is not proof
// that we do, and the conservative answer is the pre-existing refusal.
func TestHostedZone_UnreadableStateDoesNotGrantOwnership(t *testing.T) {
	fakeBin(t, "aws", `
case "$1 $2" in
  "route53 list-hosted-zones") printf 'hub.nanohype.dev.\t/hostedzone/Z999\tFalse\n' ;;
  *) exit 1 ;;
esac`)
	env := testEnv()
	env.Cfg.DNS = &config.DNS{HostedZone: "hub.nanohype.dev"}

	mustFail(t, CheckHostedZone(context.Background(), env), "create mode")
}
