package phases

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/engine"
	"github.com/rackctl/rackctl/internal/exec"
	"io"
)

// The shipped file, reduced to the shape that matters.
const providersFixture = `apiVersion: pkg.crossplane.io/v1beta1
kind: DeploymentRuntimeConfig
metadata:
  name: provider-opentofu
spec:
  serviceAccountTemplate:
    metadata:
      annotations:
        eks.amazonaws.com/role-arn: arn:aws:iam::<FLEET_ACCOUNT_ID>:role/eks-fleet-crossplane
`

func fleetState(t *testing.T, body string) *engine.State {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "config/bootstrap"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config/bootstrap/providers.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.ControlPlane.EKSFleet = true
	cfg.ControlPlane.FleetHubRoleARN = "arn:aws:iam::111111111111:role/eks-fleet-crossplane"
	return &engine.State{Config: cfg, Runner: exec.New(io.Discard), Repos: engine.Repos{EKSFleet: dir}}
}

// The placeholder must never reach the cluster.
//
// eks-fleet ships `arn:aws:iam::<FLEET_ACCOUNT_ID>:role/eks-fleet-crossplane` and its own
// stand-up doc says "the file stays a placeholder", substituting at apply time. Applying it
// unrendered annotates provider-opentofu's ServiceAccount with something EKS cannot resolve,
// so the pod gets no credentials — while the provider still reports Healthy, because
// INSTALLING it succeeded. The phase then declares success and every spoke vend fails later.
func TestRenderFleetProviders_SubstitutesTheHubRoleARN(t *testing.T) {
	st := fleetState(t, providersFixture)
	rel, err := renderFleetProviders(st)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(st.Repos.EKSFleet, rel))
	if err != nil {
		t.Fatalf("rendered file: %v", err)
	}
	got := string(b)
	if strings.Contains(got, "<FLEET_ACCOUNT_ID>") {
		t.Fatalf("the placeholder survived:\n%s", got)
	}
	if !strings.Contains(got, "eks.amazonaws.com/role-arn: arn:aws:iam::111111111111:role/eks-fleet-crossplane") {
		t.Fatalf("the hub role ARN was not substituted:\n%s", got)
	}
	// Indentation is load-bearing in YAML — a substitution that flattens it produces a
	// document that parses to something else, or not at all.
	if !strings.Contains(got, "        eks.amazonaws.com/role-arn:") {
		t.Fatalf("the annotation's indentation was not preserved:\n%s", got)
	}
}

// If upstream stops carrying that annotation, the phase must fail rather than apply a file
// whose identity wiring it no longer understands.
func TestRenderFleetProviders_RefusesWhenTheAnnotationIsGone(t *testing.T) {
	st := fleetState(t, "apiVersion: pkg.crossplane.io/v1beta1\nkind: DeploymentRuntimeConfig\n")
	if _, err := renderFleetProviders(st); err == nil {
		t.Fatal("expected a refusal — applying blind would install a provider with no identity")
	} else if !strings.Contains(err.Error(), "role-arn") {
		t.Fatalf("the error must say what is missing, got: %v", err)
	}
}

// A dry-run must not write into the checkout, and must still name the real source path.
func TestRenderFleetProviders_DryRunTouchesNothing(t *testing.T) {
	st := fleetState(t, providersFixture)
	st.Runner.DryRun = true
	rel, err := renderFleetProviders(st)
	if err != nil {
		t.Fatalf("dry-run render: %v", err)
	}
	if rel != "config/bootstrap/providers.yaml" {
		t.Fatalf("dry-run must plan against the source path, got %q", rel)
	}
	if _, err := os.Stat(filepath.Join(st.Repos.EKSFleet, ".rackctl-providers.yaml")); err == nil {
		t.Fatal("a dry-run must not write a rendered file into the checkout")
	}
}
