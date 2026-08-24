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

// The disclosure stays, and names the constraint that actually blocks a teardown.
//
// cost-pipeline declares force_destroy_buckets (components/cost-pipeline/variables.tf) and
// bedrock-account deliberately declares no such lever, deriving force_destroy from
// object_lock_mode instead — which live/org pins to GOVERNANCE, resolving it true. So the
// flag reaches both roots, and help that sends an operator to clear them by hand is help
// that describes a refusal the flag already handles.
//
// What survives is a permission, not a lever: bedrock's invocations bucket carries per-object
// GOVERNANCE retention, so the caller needs s3:BypassGovernanceRetention, which rackctl
// neither declares nor checks. A destroy without it fails at the object. That is the sentence
// an operator needs, and this is what holds it in the help.
func TestDestroyHelp_DisclosesTheBypassGovernanceRequirement(t *testing.T) {
	if !strings.Contains(destroyCmd.Long, "s3:BypassGovernanceRetention") {
		t.Error("destroy long help must name the permission a bedrock teardown requires")
	}
	for _, stale := range []string{"do not\nyet accept", "until that lands upstream"} {
		if strings.Contains(destroyCmd.Long, stale) {
			t.Errorf("destroy long help still describes the retired O5 gap: %q", stale)
		}
	}
}
