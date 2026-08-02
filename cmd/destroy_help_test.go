package cmd

import (
	"strings"
	"testing"
)

// The druid carve-out is gone from the operator-facing text, not just from the code.
//
// A refusal that no longer happens but is still documented is worse than one that never
// existed: it sends an operator to clear a gate by hand that the flag already clears, and it
// makes `--force-buckets` look narrower than it is. This repo has been bitten by exactly that
// direction before — flag help that claimed a teardown worked where it did not — so the rule is
// that behaviour and help change in the same commit, and this is what holds it.
func TestDestroyHelp_DoesNotCarryTheRetiredDruidCarveOut(t *testing.T) {
	for _, claim := range []string{"ledger O8", "NOT covered outside development", "clear deletion_protection out of band"} {
		if strings.Contains(destroyCmd.Long, claim) {
			t.Errorf("destroy long help still carries the retired druid carve-out: %q", claim)
		}
	}
	flag := destroyCmd.Flags().Lookup("force-buckets")
	if flag == nil {
		t.Fatal("--force-buckets is gone")
	}
	if strings.Contains(flag.Usage, "does not cover druid") {
		t.Errorf("--force-buckets help still says it does not cover druid:\n%s", flag.Usage)
	}
}

// The O5 disclosure STAYS. bedrock and cost-pipeline still have no force_destroy_buckets
// upstream, so a destroy after the platform has run can still wedge there. Retiring one
// carve-out must not quietly retire its neighbour — each has its own owner and its own end.
func TestDestroyHelp_StillDisclosesTheAgentPlatformGap(t *testing.T) {
	if !strings.Contains(destroyCmd.Long, "O5") {
		t.Error("the eks-agent-platform bedrock/cost-pipeline gap (ledger O5) is still open " +
			"upstream and must still be disclosed")
	}
}
