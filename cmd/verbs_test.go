package cmd

import (
	"strings"
	"testing"
)

// The verb — not a flag — decides whether anything is written.
//
// This replaced `init` + `--apply`, where the destructive invocation differed from the safe one
// by five characters at the END of a line: read last, copied first, and indistinguishable in a
// shell history or a runbook until you get to the end. And `init` is the word every reader of
// terraform or git takes to mean the cheap, local, idempotent thing — here it built a VPC and an
// EKS control plane.
func TestVerbs_AreThePlanApplyDestroySet(t *testing.T) {
	got := map[string]bool{}
	for _, c := range rootCmd.Commands() {
		got[c.Name()] = true
	}
	for _, want := range []string{"plan", "apply", "destroy", "preflight", "doctor", "upgrade", "version"} {
		if !got[want] {
			t.Errorf("command %q is not registered", want)
		}
	}
	// `init` must be GONE, not aliased. A deprecation shim would keep the confusing verb alive
	// and let a stale runbook keep working while meaning something different.
	if got["init"] {
		t.Error("`init` is still registered — the rename is a replacement, not an alias")
	}
}

// No write command may carry an --apply flag. `rackctl apply --apply` is the absurdity that
// forced the rename; re-adding it anywhere would reintroduce the shape.
func TestVerbs_NoCommandCarriesAnApplyFlag(t *testing.T) {
	for _, c := range rootCmd.Commands() {
		if f := c.Flags().Lookup("apply"); f != nil {
			t.Errorf("%q has an --apply flag; the verb is what decides whether anything is written", c.Name())
		}
	}
}

// plan must be read-only, and it must say so. This is the property the whole shape rests on: if
// plan can write, naming the intent as the verb has bought nothing.
func TestPlan_IsReadOnlyAndSaysSo(t *testing.T) {
	if !strings.Contains(planCmd.Long, "Read-only") {
		t.Error("plan's help must state that it is read-only")
	}
	if strings.Contains(strings.ToLower(applyCmd.Long), "dry-run") {
		t.Error("apply's help must not describe itself as a dry-run — it writes")
	}
	// apply has to be explicit that it spends money. `init` was not, which is why it was wrong.
	if !strings.Contains(applyCmd.Long, "spends real money") {
		t.Error("apply's help must say plainly that it spends real money")
	}
}

// destroy lost --apply, so something had to take over that flag's job. --yes is for CI; the
// interactive path asks for the cluster NAME rather than y/n, because y/n is answered by reflex
// and the failure this guards is a reflex failure.
func TestDestroy_HasAConfirmationPathAndADryRun(t *testing.T) {
	for _, want := range []string{"yes", "dry-run", "force-buckets"} {
		if destroyCmd.Flags().Lookup(want) == nil {
			t.Errorf("destroy is missing --%s", want)
		}
	}
	if !strings.Contains(destroyCmd.Long, "--yes") {
		t.Error("destroy's help must name --yes, or a scripted teardown has no documented path")
	}
}
