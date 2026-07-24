package phases

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/engine"
	"github.com/rackctl/rackctl/internal/exec"
)

// fakeTools puts recording `helm` and `kubectl` stubs on PATH and returns a reader for
// the invocation log. The Runner shells out by name, so this is the seam — same idiom as
// fakeGH here and fakeKubectl in internal/doctor.
//
// The runner must NOT be in dry-run: dry-run prints the command instead of executing it,
// so nothing would reach the stub and the argv would go unasserted — which is exactly how
// the bug this file pins survived.
func fakeTools(t *testing.T) (*engine.State, func() []string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$(basename \"$0\") $*\" >> %q\nexit 0\n", logPath)

	for _, tool := range []string{"helm", "kubectl"} {
		if err := os.WriteFile(filepath.Join(dir, tool), []byte(script), 0o755); err != nil {
			t.Fatalf("write fake %s: %v", tool, err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := baseCfg()
	cfg.FirstTenant = &config.FirstTenant{
		Name: "blank", Persona: "generic", Tenant: "example", MonthlyBudgetUSD: 100,
	}
	cfg.AgentPlatform.BedrockModelFamilies = []string{"anthropic", "amazon-nova"}
	cfg.AgentPlatform.Compliance = config.Compliance{SOC2: true}
	st := &engine.State{Config: cfg, Runner: exec.New(io.Discard)}

	return st, func() []string {
		b, err := os.ReadFile(logPath)
		if err != nil {
			return nil // never invoked
		}
		var calls []string
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if l != "" {
				calls = append(calls, l)
			}
		}
		return calls
	}
}

func callStartingWith(t *testing.T, calls []string, prefix string) string {
	t.Helper()
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return c
		}
	}
	t.Fatalf("no %q invocation; got %v", prefix, calls)
	return ""
}

// charts/tenant nests its values under `platform.` and hard-fails the render on a missing
// platform.name. rackctl used to pass bare `tenant=`/`persona=` and never passed
// platform.name at all, so helm accepted three orphan values no template reads and the
// render died before creating a single object. --set on an unread path is silent, which is
// why it shipped — so the argv is pinned here, and a key rename upstream breaks a unit
// test in a second instead of a real bootstrap an hour in.
func TestSmoke_HelmSetKeysMatchTheTenantChart(t *testing.T) {
	st, calls := fakeTools(t)
	if err := (smoke{}).Run(context.Background(), st); err != nil {
		t.Fatalf("smoke.Run: %v", err)
	}
	got := callStartingWith(t, calls(), "helm upgrade")

	for _, want := range []string{
		"--set platform.name=blank",
		"--set platform.tenant=example",
		"--set platform.persona=generic",
		"--set budget.monthlyUsd=100",
		"--set controlPlaneNamespace=eks-agent-platform",
		"--namespace eks-agent-platform",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("helm invocation missing %q; got: %s", want, got)
		}
	}
	// The orphan keys must not come back. They bind to nothing.
	for _, bad := range []string{"--set tenant=", "--set persona="} {
		if strings.Contains(got, bad) {
			t.Errorf("helm invocation still passes the orphan key %q (no template reads it); got: %s", bad, got)
		}
	}
}

// The config's model boundary and compliance flags must reach the tenant. Passing
// neither vends the first tenant against the chart's own defaults, so a config asking
// for hipaa or a narrowed model family would produce a Platform that silently
// contradicts it — in the very phase that exists to prove vending works.
func TestSmoke_PassesTheConfigsModelBoundaryAndCompliance(t *testing.T) {
	st, calls := fakeTools(t)
	if err := (smoke{}).Run(context.Background(), st); err != nil {
		t.Fatalf("smoke.Run: %v", err)
	}
	got := callStartingWith(t, calls(), "helm upgrade")

	for _, want := range []string{
		"--set identity.allowedModelFamilies={anthropic,amazon-nova}",
		"--set platform.compliance.soc2=true",
		"--set platform.compliance.hipaa=false",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("helm invocation missing %q — the tenant would be vended against chart defaults "+
				"that can contradict the config; got: %s", want, got)
		}
	}
}

// A Platform never gets a Ready CONDITION — the operator reports readiness as
// status.phase == "Ready", and writes only Suspended / ModelAccessScoped / NamespaceReady
// / VClusterReady. Waiting on the condition blocks the full 15m against a tenant that is
// already up, then fails the phase. And the chart plants its CRs in controlPlaneNamespace,
// not the release namespace, so the wait must name it.
func TestSmoke_WaitsOnStatusPhaseNotANonexistentCondition(t *testing.T) {
	st, calls := fakeTools(t)
	if err := (smoke{}).Run(context.Background(), st); err != nil {
		t.Fatalf("smoke.Run: %v", err)
	}
	got := callStartingWith(t, calls(), "kubectl")

	for _, want := range []string{
		"-n eks-agent-platform",
		"--for=jsonpath={.status.phase}=Ready",
		"platform/blank",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("kubectl wait missing %q; got: %s", want, got)
		}
	}
	if strings.Contains(got, "--for=condition=Ready") {
		t.Errorf("kubectl waits on a condition the operator never writes; got: %s", got)
	}
}

// The tenant must be removed before the substrate under it is, so the operator's finalizer
// can drop the per-tenant IAM role while the operator is still alive. A surviving role
// attaches the tenant baseline policy that agent-iam's destroy must delete, and that fails
// DeleteConflict — halting the teardown with the cluster half-gone.
func TestSmoke_TeardownUninstallsTheTenant(t *testing.T) {
	st, calls := fakeTools(t)
	if err := (smoke{}).Teardown(context.Background(), st); err != nil {
		t.Fatalf("smoke.Teardown: %v", err)
	}
	got := callStartingWith(t, calls(), "helm uninstall")
	for _, want := range []string{"blank", "-n eks-agent-platform", "--ignore-not-found"} {
		if !strings.Contains(got, want) {
			t.Errorf("helm uninstall missing %q; got: %s", want, got)
		}
	}
}
