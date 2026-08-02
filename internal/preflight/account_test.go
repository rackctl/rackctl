package preflight

import (
	"context"
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

// The Bedrock API holds exactly one invocation-logging configuration per account per region.
// Under rackctl every environment shares one account, so applying here silently takes over
// whichever environment configured it last — and a teardown deletes it outright.
func TestBedrockLogging_AnotherClustersSingletonRefuses(t *testing.T) {
	fakeBin(t, "aws", `
case "$2" in
  get-model-invocation-logging-configuration) echo "production-platform-bedrock-invocations-111122223333" ;;
  *) exit 1 ;;
esac`)
	r := CheckBedrockLogging(context.Background(), testEnv())
	mustFail(t, r, "ONE such configuration per")
	if !strings.Contains(r.Detail, "production-platform-bedrock-invocations") {
		t.Errorf("the failure must name the CURRENT owner, or the operator cannot tell whose "+
			"logging they are about to take:\n%s", r.Detail)
	}
}

// Already ours is a no-op, not a takeover. Without this the check would refuse every re-apply.
func TestBedrockLogging_OwnSingletonIsFine(t *testing.T) {
	fakeBin(t, "aws", `
case "$2" in
  get-model-invocation-logging-configuration) echo "development-platform-bedrock-invocations-111122223333" ;;
  *) exit 1 ;;
esac`)
	if r := CheckBedrockLogging(context.Background(), testEnv()); r.Status != doctor.OK {
		t.Fatalf("re-applying against our own singleton must pass.\ngot %s: %s", r.Status, r.Detail)
	}
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
