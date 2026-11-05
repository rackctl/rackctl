package preflight

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/doctor"
)

func skewEnv(t *testing.T, want string) *Env {
	t.Helper()
	e := testEnv()
	e.Cfg.Cluster.Version = want
	return e
}

// EKS upgrades the control plane ONE minor at a time. A two-minor jump is rejected — in the
// cluster phase, after the run has started — and the error names neither where the cluster is
// now nor the sequence out.
func TestVersionSkew_MultiMinorJumpNamesTheSequence(t *testing.T) {
	fakeBin(t, "aws", `case "$2" in describe-cluster) echo 1.34 ;; *) exit 1 ;; esac`)
	r := CheckVersionSkew(context.Background(), skewEnv(t, "1.37"))
	mustFail(t, r, "one minor version at a time")
	for _, step := range []string{"1.35", "1.36", "1.37"} {
		if !strings.Contains(r.Detail, step) {
			t.Errorf("the remedy must spell out the sequence; %s missing:\n%s", step, r.Detail)
		}
	}
}

// The one that cannot be undone. There is no EKS downgrade, so this is not an error the
// operator can act on afterwards — it has to be caught before the apply.
func TestVersionSkew_DowngradeIsRefused(t *testing.T) {
	fakeBin(t, "aws", `case "$2" in describe-cluster) echo 1.36 ;; *) exit 1 ;; esac`)
	r := CheckVersionSkew(context.Background(), skewEnv(t, "1.34"))
	mustFail(t, r, "cannot downgrade")
}

// A single step is the whole point of the feature and must pass, or the check blocks every
// legitimate upgrade.
func TestVersionSkew_SingleMinorStepIsAllowed(t *testing.T) {
	fakeBin(t, "aws", `case "$2" in describe-cluster) echo 1.36 ;; *) exit 1 ;; esac`)
	if r := CheckVersionSkew(context.Background(), skewEnv(t, "1.37")); r.Status != doctor.OK {
		t.Fatalf("1.36 → 1.37 is exactly what EKS allows.\ngot %s: %s", r.Status, r.Detail)
	}
}

// Same version is a re-apply, not an upgrade.
func TestVersionSkew_SameVersionIsFine(t *testing.T) {
	fakeBin(t, "aws", `case "$2" in describe-cluster) echo 1.37 ;; *) exit 1 ;; esac`)
	if r := CheckVersionSkew(context.Background(), skewEnv(t, "1.37")); r.Status != doctor.OK {
		t.Fatalf("re-applying the current version must pass.\ngot %s: %s", r.Status, r.Detail)
	}
}

// A fresh install has no skew — any supported version is a valid starting point. Failing here
// would block every first install, which is the majority case.
//
// The fixture is what EKS actually answers for an absent cluster, not a bare non-zero exit:
// the two are different answers and this check now tells them apart.
func TestVersionSkew_NoClusterIsNotAFailure(t *testing.T) {
	fakeBin(t, "aws", `echo "An error occurred (ResourceNotFoundException) when calling the DescribeCluster operation: No cluster found" >&2; exit 254`)
	if r := CheckVersionSkew(context.Background(), skewEnv(t, "1.36")); r.Status != doctor.OK {
		t.Fatalf("no live cluster means nothing to compare against.\ngot %s: %s", r.Status, r.Detail)
	}
}

// An unreadable cluster is NOT an absent one, and the difference is the whole gate.
//
// AccessDenied, a throttle and an expired token would each otherwise render as "no live
// cluster — any supported version is a valid starting point", passing the one check standing
// between an operator and an irreversible EKS upgrade. Unknown must say unknown.
func TestVersionSkew_UnreadableClusterIsNotReportedAsAbsent(t *testing.T) {
	for _, stderr := range []string{
		"An error occurred (AccessDeniedException) when calling the DescribeCluster operation",
		"An error occurred (ThrottlingException) when calling the DescribeCluster operation",
		"ExpiredToken: The security token included in the request is expired",
	} {
		fakeBin(t, "aws", `echo `+strconv.Quote(stderr)+` >&2; exit 254`)
		r := CheckVersionSkew(context.Background(), skewEnv(t, "1.36"))
		if r.Status == doctor.OK {
			t.Errorf("%q was reported as a clean pass — an unanswered question must not read "+
				"as a healthy answer.\ngot: %s", stderr, r.Detail)
		}
		if !strings.Contains(r.Detail, "UNCHECKED") {
			t.Errorf("the operator must be told the gate did not run.\ngot: %s", r.Detail)
		}
	}
}

// A cluster.version equal to the default is never sent, so the leaf's own default is what
// the cluster phase applies. The gate cannot read that, so it says the skew went unverified,
// and never refuses on rackctl's mirror of a number the apply does not use — whether the
// mirror would look like a jump, a downgrade or a match.
func TestVersionSkew_TheDefaultIsUnverifiedNeverRefused(t *testing.T) {
	def := config.Default().Cluster.Version
	for _, live := range []string{"1.30", def, "1.99"} {
		fakeBin(t, "aws", `case "$2" in describe-cluster) echo `+live+` ;; *) exit 1 ;; esac`)
		for _, want := range []string{def, ""} {
			r := CheckVersionSkew(context.Background(), skewEnv(t, want))
			if r.Status != doctor.Warn || !strings.Contains(r.Detail, "UNVERIFIED") {
				t.Errorf("live %s, cluster.version %q: got %s %s — the default is not sent, so "+
					"its skew is unverified", live, want, r.Status, r.Detail)
			}
		}
	}
}

// A fresh install on the default has nothing to compare and passes.
func TestVersionSkew_TheDefaultOnAFreshInstallPasses(t *testing.T) {
	fakeBin(t, "aws", eksAbsent)
	if r := CheckVersionSkew(context.Background(), skewEnv(t, config.Default().Cluster.Version)); r.Status != doctor.OK {
		t.Errorf("no live cluster: got %s %s", r.Status, r.Detail)
	}
}
