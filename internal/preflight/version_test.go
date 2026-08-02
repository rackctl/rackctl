package preflight

import (
	"context"
	"strings"
	"testing"

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
	fakeBin(t, "aws", `case "$2" in describe-cluster) echo 1.33 ;; *) exit 1 ;; esac`)
	r := CheckVersionSkew(context.Background(), skewEnv(t, "1.36"))
	mustFail(t, r, "one minor version at a time")
	for _, step := range []string{"1.34", "1.35", "1.36"} {
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
	fakeBin(t, "aws", `case "$2" in describe-cluster) echo 1.35 ;; *) exit 1 ;; esac`)
	if r := CheckVersionSkew(context.Background(), skewEnv(t, "1.36")); r.Status != doctor.OK {
		t.Fatalf("1.35 → 1.36 is exactly what EKS allows.\ngot %s: %s", r.Status, r.Detail)
	}
}

// Same version is a re-apply, not an upgrade.
func TestVersionSkew_SameVersionIsFine(t *testing.T) {
	fakeBin(t, "aws", `case "$2" in describe-cluster) echo 1.36 ;; *) exit 1 ;; esac`)
	if r := CheckVersionSkew(context.Background(), skewEnv(t, "1.36")); r.Status != doctor.OK {
		t.Fatalf("re-applying the current version must pass.\ngot %s: %s", r.Status, r.Detail)
	}
}

// A fresh install has no skew — any supported version is a valid starting point. Failing here
// would block every first install, which is the majority case.
func TestVersionSkew_NoClusterIsNotAFailure(t *testing.T) {
	fakeBin(t, "aws", `exit 1`)
	if r := CheckVersionSkew(context.Background(), skewEnv(t, "1.36")); r.Status != doctor.OK {
		t.Fatalf("no live cluster means nothing to compare against.\ngot %s: %s", r.Status, r.Detail)
	}
}
