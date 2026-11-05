package preflight

import (
	"context"
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/doctor"
)

// "I could not find out" is not "there is nothing there". Each test below hands a check a
// read that was DENIED, and asserts the verdict is a warning naming what went unchecked and
// carrying the tool's reason — never the ok that a skipped read used to produce.

func mustWarn(t *testing.T, r doctor.Result, wantSubstr ...string) {
	t.Helper()
	if r.Status != doctor.Warn {
		t.Fatalf("%s: want Warn for a read that did not answer, got %s:\n%s", r.Name, r.Status, r.Detail)
	}
	for _, w := range wantSubstr {
		if !strings.Contains(r.Detail, w) {
			t.Errorf("%s: detail is missing %q:\n%s", r.Name, w, r.Detail)
		}
	}
}

// ─────────────────────────── stale state ───────────────────────────

// A denied state read is not an absent state object. Reading it as one reported "no state —
// a clean provision" over state nobody looked at.
func TestCheckStaleState_DeniedStateReadIsUnchecked(t *testing.T) {
	fakeBin(t, "aws", `
case "$1 $2" in
  "eks describe-cluster") `+eksAbsent+` ;;
  "s3 cp")                `+denied+` ;;
esac`)
	mustWarn(t, CheckStaleState(context.Background(), testEnv()), "UNCHECKED", "AccessDenied", "network")
}

// A 404 IS absence, and a clean account must still read as one.
func TestCheckStaleState_NotFoundIsAbsence(t *testing.T) {
	fakeBin(t, "aws", `
case "$1 $2" in
  "eks describe-cluster") `+eksAbsent+` ;;
  "s3 cp")                `+s3Absent+` ;;
esac`)
	r := CheckStaleState(context.Background(), testEnv())
	if r.Status != doctor.OK {
		t.Fatalf("a 404 on every state key is a clean account: %s — %s", r.Status, r.Detail)
	}
	// The agent platform defaults on, and its own backend is not read here. The verdict must
	// say so rather than let "no state" read as a statement about that tree too.
	if !strings.Contains(r.Detail, "eks-agent-platform-tfstate-111122223333-us-west-2") {
		t.Errorf("the ok must name the backend it did not cover:\n%s", r.Detail)
	}
}

// State that does not parse is a state object this check did not read.
func TestCheckStaleState_UnparseableStateIsUnchecked(t *testing.T) {
	fakeBin(t, "aws", `
case "$1 $2" in
  "eks describe-cluster") `+eksAbsent+` ;;
  "s3 cp")                echo 'not json' ;;
esac`)
	mustWarn(t, CheckStaleState(context.Background(), testEnv()), "not readable JSON")
}

// A denied describe-cluster is not a missing cluster. Reading it as one turned a re-apply
// into a failure telling the operator to destroy a platform that is standing.
func TestCheckStaleState_DeniedClusterReadIsNotAGoneCluster(t *testing.T) {
	fakeBin(t, "aws", `
case "$1 $2" in
  "eks describe-cluster") `+denied+` ;;
  "s3 cp")                echo '{"resources":[{"type":"aws_vpc"}]}' ;;
esac`)
	mustWarn(t, CheckStaleState(context.Background(), testEnv()), "AccessDenied", "UNCHECKED")
}

// The failure still carries what it could not read, so a partial picture is not mistaken
// for a whole one.
func TestCheckStaleState_FailureNamesUnreadComponents(t *testing.T) {
	fakeBin(t, "aws", `
case "$1 $2" in
  "eks describe-cluster") `+eksAbsent+` ;;
  "s3 cp") case "$3" in
             */network/*) `+denied+` ;;
             *) echo '{"resources":[{"type":"aws_vpc"}]}' ;;
           esac ;;
esac`)
	r := CheckStaleState(context.Background(), testEnv())
	mustFail(t, r, "rackctl destroy")
	if !strings.Contains(r.Detail, "network (") || !strings.Contains(r.Detail, "UNCHECKED") {
		t.Errorf("the failure must name the component whose state went unread:\n%s", r.Detail)
	}
}

// ─────────────────────────── collisions ───────────────────────────

// A probe that errored did not look, and "no orphans" over the probes that did is a verdict
// resting on fewer observations than it claims.
func TestCheckCollisions_DeniedProbeIsUnchecked(t *testing.T) {
	fakeBin(t, "aws", `
case "$1 $2" in
  "eks describe-cluster") `+eksAbsent+` ;;
  "kms list-aliases")     `+denied+` ;;
  *)                      echo 0 ;;
esac`)
	mustWarn(t, CheckCollisions(context.Background(), testEnv()), "UNCHECKED", "AccessDenied", "KMS alias/eks/development-platform")
}

// A count that does not parse is a probe that did not answer, not a zero.
func TestCheckCollisions_UnreadableCountIsUnchecked(t *testing.T) {
	fakeBin(t, "aws", `
case "$1 $2" in
  "eks describe-cluster") `+eksAbsent+` ;;
  "iam list-roles")       echo 'garbage' ;;
  *)                      echo 0 ;;
esac`)
	mustWarn(t, CheckCollisions(context.Background(), testEnv()), "unreadable count", "IAM roles")
}

// Every probe answering zero is the clean case.
func TestCheckCollisions_AllZeroIsClear(t *testing.T) {
	fakeBin(t, "aws", `
case "$1 $2" in
  "eks describe-cluster") `+eksAbsent+` ;;
  *)                      echo 0 ;;
esac`)
	if r := CheckCollisions(context.Background(), testEnv()); r.Status != doctor.OK {
		t.Fatalf("every probe answered zero: %s — %s", r.Status, r.Detail)
	}
}

// A found orphan still fails when another probe went unread, and says so.
func TestCheckCollisions_FailureNamesUnreadProbes(t *testing.T) {
	fakeBin(t, "aws", `
case "$1 $2" in
  "eks describe-cluster") `+eksAbsent+` ;;
  "kms list-aliases")     echo 1 ;;
  "sqs list-queues")      `+denied+` ;;
  *)                      echo 0 ;;
esac`)
	r := CheckCollisions(context.Background(), testEnv())
	mustFail(t, r, "SQS Karpenter-development-platform (")
}

// The queue probe supplies its own empty list, because an account with no queues answers
// with no QueueUrls key and length() of a missing key is a query error.
func TestCheckCollisions_QueueProbeToleratesNoQueues(t *testing.T) {
	fakeBin(t, "aws", `
case "$1 $2" in
  "eks describe-cluster") `+eksAbsent+` ;;
  "sqs list-queues") case "$*" in *'|| `+"`[]`"+`'*) echo 0 ;; *) exit 255 ;; esac ;;
  *)                      echo 0 ;;
esac`)
	if r := CheckCollisions(context.Background(), testEnv()); r.Status != doctor.OK {
		t.Fatalf("no queues is a zero, not a failed probe: %s — %s", r.Status, r.Detail)
	}
}

// ─────────────────────────── bucket names ───────────────────────────

// Without list-buckets, a bucket that exists cannot be told apart as this account's or
// another's. The old code read the failure as "we own nothing" and reported names it never
// classified.
func TestBucketNames_DeniedListIsUnchecked(t *testing.T) {
	fakeBin(t, "aws", `
case "$1 $2" in
  "s3api list-buckets")   `+denied+` ;;
  "eks describe-cluster") `+eksAbsent+` ;;
  "s3api head-bucket")    `+s3Absent+` ;;
esac`)
	mustWarn(t, CheckBucketNames(context.Background(), testEnv()), "UNCHECKED", "AccessDenied")
}

// A 403 from head-bucket is a bucket another account owns. Reading it as free waved through
// the one outcome no cleanup here can fix.
func TestBucketNames_ForbiddenHeadIsAnotherAccountsBucket(t *testing.T) {
	fakeBin(t, "aws", `
case "$1 $2" in
  "s3api list-buckets")   echo "" ;;
  "eks describe-cluster") `+eksAbsent+` ;;
  "s3api head-bucket")    echo "An error occurred (403) when calling the HeadBucket operation: Forbidden" >&2; exit 254 ;;
esac`)
	r := CheckBucketNames(context.Background(), testEnv())
	mustFail(t, r, "NOT owned by this account")
}

// A head-bucket that neither found nor refused did not answer.
func TestBucketNames_UnansweredHeadIsUnchecked(t *testing.T) {
	fakeBin(t, "aws", `
case "$1 $2" in
  "s3api list-buckets")   echo "" ;;
  "eks describe-cluster") `+eksAbsent+` ;;
  "s3api head-bucket")    echo "Could not connect to the endpoint URL" >&2; exit 255 ;;
esac`)
	mustWarn(t, CheckBucketNames(context.Background(), testEnv()), "UNCHECKED", "Could not connect")
}

// Leftover buckets with an unreadable cluster are wreckage or a re-apply, and the check
// cannot tell which.
func TestBucketNames_LeftoverWithUnknownClusterIsUnchecked(t *testing.T) {
	fakeBin(t, "aws", `
case "$1 $2" in
  "s3api list-buckets")   echo "development-platform-111122223333-us-west-2-velero" ;;
  "eks describe-cluster") `+denied+` ;;
  "s3api head-bucket")    `+s3Absent+` ;;
esac`)
	mustWarn(t, CheckBucketNames(context.Background(), testEnv()), "velero", "AccessDenied")
}

// A name another account owns is decided by S3 alone. A leftover of this account's and an
// unreadable cluster beside it must not soften that refusal into the unchecked warning.
func TestBucketNames_AnotherAccountsBucketFailsWhateverTheClusterRead(t *testing.T) {
	fakeBin(t, "aws", `
case "$1 $2" in
  "s3api list-buckets")   echo "development-platform-111122223333-us-west-2-velero" ;;
  "eks describe-cluster") `+denied+` ;;
  "s3api head-bucket")
    case "$4" in
      *-loki) echo "An error occurred (403) when calling the HeadBucket operation: Forbidden" >&2; exit 254 ;;
      *)      `+s3Absent+` ;;
    esac ;;
esac`)
	r := CheckBucketNames(context.Background(), testEnv())
	mustFail(t, r, "NOT owned by this account")
	if !strings.Contains(r.Detail, "-loki") {
		t.Errorf("the refusal must name the taken bucket:\n%s", r.Detail)
	}
}

// An account-scoped bucket alongside an unanswered probe still warns, and names both.
func TestBucketNames_SharedWarningCarriesUnansweredProbes(t *testing.T) {
	fakeBin(t, "aws", `
case "$1 $2" in
  "s3api list-buckets")   echo "org-111122223333-us-west-2-bedrock-invocations" ;;
  "eks describe-cluster") `+eksAbsent+` ;;
  "s3api head-bucket")    echo "Could not connect to the endpoint URL" >&2; exit 255 ;;
esac`)
	mustWarn(t, CheckBucketNames(context.Background(), testEnv()), "bedrock-invocations", "UNCHECKED")
}

// ─────────────────────────── bedrock logging ───────────────────────────

// Unread is not unconfigured: the singleton is account-global and a denied read used to
// report "no invocation logging configured".
func TestBedrockLogging_DeniedReadIsUnchecked(t *testing.T) {
	fakeBin(t, "aws", denied)
	mustWarn(t, CheckBedrockLogging(context.Background(), testEnv()), "UNCHECKED", "AccessDenied")
}

// ─────────────────────────── session lifetime ───────────────────────────

// An export that FAILS is an unread session, not a session with no expiry.
func TestSessionLifetime_FailedExportIsUnchecked(t *testing.T) {
	fakeBin(t, "aws", `echo "Error when retrieving token from sso: Token has expired and refresh failed" >&2; exit 255`)
	mustWarn(t, CheckSessionLifetime(context.Background(), testEnv()), "UNCHECKED", "Token has expired")
}

// Exported credentials that do not parse were not read.
func TestSessionLifetime_UnparseableExportIsUnchecked(t *testing.T) {
	fakeBin(t, "aws", `echo 'not json'`)
	mustWarn(t, CheckSessionLifetime(context.Background(), testEnv()), "UNCHECKED")
}

// ─────────────────────────── local vend ───────────────────────────

// A fetch that failed compared nothing. The old loop `continue`d past it and still reported
// "checkouts are current".
func TestCheckVendFreshness_FailedFetchIsUnchecked(t *testing.T) {
	env := vendEnv(t, `printf '0\t0\n'`)
	fakeBin(t, "git", `
case "$1 $2" in
  "rev-parse --git-dir") echo .git; exit 0 ;;
  "fetch --quiet")       echo "fatal: unable to access remote" >&2; exit 128 ;;
esac
printf '0\t0\n'`)
	mustWarn(t, CheckVendFreshness(context.Background(), env), "UNCHECKED", "landing-zone", "unable to access")
}

// A failed rev-list and an unreadable count are both comparisons that did not happen.
func TestCheckVendFreshness_UnreadableComparisonIsUnchecked(t *testing.T) {
	env := vendEnv(t, `case "$repo" in landing-zone) echo "fatal: no upstream" >&2; exit 128 ;; eks-gitops) echo garbage ;; *) printf '0\t0\n' ;; esac`)
	r := CheckVendFreshness(context.Background(), env)
	mustWarn(t, r, "landing-zone (cannot compare", "eks-gitops (unreadable", "1 of 3")
}

// Nothing cloned yet is a fresh machine, and the verdict says that rather than "current".
func TestCheckVendFreshness_NothingClonedSaysSo(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fakeBin(t, "git", `exit 128`)
	r := CheckVendFreshness(context.Background(), testEnv())
	if r.Status != doctor.OK || !strings.Contains(r.Detail, "nothing cloned yet") {
		t.Fatalf("an empty workdir is a fresh install: %s — %s", r.Status, r.Detail)
	}
}

// Every examined repo current names how many were examined.
func TestCheckVendFreshness_CurrentCountsWhatItCompared(t *testing.T) {
	env := vendEnv(t, `printf '0\t0\n'`)
	r := CheckVendFreshness(context.Background(), env)
	if r.Status != doctor.OK || !strings.Contains(r.Detail, "3 checkout(s)") {
		t.Fatalf("three repos compared and current: %s — %s", r.Status, r.Detail)
	}
}

// Lag alongside an unchecked repo names both.
func TestCheckVendFreshness_LagCarriesUnchecked(t *testing.T) {
	env := vendEnv(t, `case "$repo" in landing-zone) printf '0\t2\n' ;; eks-gitops) echo garbage ;; *) printf '0\t0\n' ;; esac`)
	mustWarn(t, CheckVendFreshness(context.Background(), env), "landing-zone (2)", "eks-gitops (unreadable")
}

// ─────────────────────────── not applicable ───────────────────────────

// A check that does not apply asked nothing, so it reports skip — not an ok that reads as a
// question answered well.
func TestNotApplicableChecksReportSkip(t *testing.T) {
	fakeBin(t, "aws", `exit 1`)
	off := false

	noAP := testEnv()
	noAP.Cfg.AgentPlatform.Enable = &off

	noDNS := testEnv()
	noDNS.Cfg.DNS = nil

	for _, tc := range []struct {
		name string
		r    doctor.Result
	}{
		{"hosted zone without a dns block", CheckHostedZone(context.Background(), noDNS)},
		{"bedrock logging with the agent platform off", CheckBedrockLogging(context.Background(), noAP)},
		{"cost allocation with the agent platform off", CheckCostAllocationTags(context.Background(), noAP)},
		{"cost pipeline with the agent platform off", CheckCostPipelinePrerequisite(context.Background(), noAP)},
	} {
		if tc.r.Status != doctor.Skip {
			t.Errorf("%s: want skip, got %s — %s", tc.name, tc.r.Status, tc.r.Detail)
		}
	}
	if doctor.Failed([]doctor.Result{{Status: doctor.Skip}}) {
		t.Error("skip must not fail a run — nothing failed")
	}
}
