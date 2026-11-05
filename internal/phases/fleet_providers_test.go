package phases

import (
	"context"
	"errors"
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
	path, cleanup, err := renderFleetProviders(st)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("rendered file: %v", err)
	}
	// Outside the checkout and applied by absolute path: an untracked file in the checkout
	// survives `git checkout .` and may collide with whatever upstream does not ignore.
	if !filepath.IsAbs(path) || strings.HasPrefix(path, st.Repos.EKSFleet+string(filepath.Separator)) {
		t.Errorf("the rendered file must be an absolute path outside the checkout, got %s", path)
	}
	entries, _ := os.ReadDir(st.Repos.EKSFleet)
	for _, e := range entries {
		if e.Name() != "config" {
			t.Errorf("render wrote %s into the eks-fleet checkout", e.Name())
		}
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("cleanup must remove the rendered file: %v", err)
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
	if _, _, err := renderFleetProviders(st); err == nil {
		t.Fatal("expected a refusal — applying blind would install a provider with no identity")
	} else if !strings.Contains(err.Error(), "role-arn") {
		t.Fatalf("the error must say what is missing, got: %v", err)
	}
}

// A dry-run must not write into the checkout, and must still name the real source path.
func TestRenderFleetProviders_DryRunTouchesNothing(t *testing.T) {
	st := fleetState(t, providersFixture)
	st.Runner.DryRun = true
	rel, _, err := renderFleetProviders(st)
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

// The locator for a WRITE path, controlled in both directions and for its position.
//
// Three properties, each of which a plausible pattern gets wrong: a commented annotation
// above the live one must not win the match; the match must start at the annotation's own
// line rather than on a blank line above it; and the live annotation must be rewritten.
func TestFleetRoleARNLine_LocatesTheLiveAnnotationAndOnlyThat(t *testing.T) {
	body := "  annotations:\n" +
		"    # superseded: eks.amazonaws.com/role-arn: arn:aws:iam::<FLEET_ACCOUNT_ID>:role/old\n" +
		"\n" +
		"    eks.amazonaws.com/role-arn: arn:aws:iam::<FLEET_ACCOUNT_ID>:role/live\n"

	if got := len(fleetRoleARNLine.FindAllString(body, -1)); got != 1 {
		t.Fatalf("matched %d lines, want 1 — a commented annotation above the live one must "+
			"not win the match", got)
	}

	// The match must begin at the annotation's own line start. \s* would begin it on the
	// blank line above and report a position that is not the annotation's.
	want := strings.Index(body, "    eks.amazonaws.com/role-arn:")
	if got := fleetRoleARNLine.FindStringIndex(body)[0]; got != want {
		t.Errorf("match starts at byte %d, want %d — the pattern swallowed the blank line "+
			"above the annotation, so any position it reports is not the annotation's", got, want)
	}

	out := fleetRoleARNLine.ReplaceAllString(body, "${1}eks.amazonaws.com/role-arn: NEW")
	if !strings.Contains(out, "    eks.amazonaws.com/role-arn: NEW") {
		t.Errorf("the live annotation was not rewritten:\n%s", out)
	}
	if !strings.Contains(out, "role/old") {
		t.Errorf("the commented annotation was rewritten:\n%s", out)
	}
}

// More than one annotation is a file that wires more than one identity. Substituting the hub
// role into every one would hand it to whatever the others are.
func TestRenderFleetProviders_RefusesMoreThanOneAnnotation(t *testing.T) {
	st := fleetState(t, providersFixture+"---\n"+providersFixture)
	_, _, err := renderFleetProviders(st)
	var nr *engine.NoRollbackError
	if !errors.As(err, &nr) || !strings.Contains(err.Error(), "2 eks.amazonaws.com/role-arn") {
		t.Fatalf("two annotations must refuse, naming the count; got %v", err)
	}
}

// fakeIAM puts an aws on PATH that answers get-role with body.
func fakeIAM(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	if err := os.WriteFile(filepath.Join(dir, "aws"), []byte("#!/bin/sh\necho \"$*\" >> "+log+"\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

const noSuchRole = `echo "An error occurred (NoSuchEntity) when calling the GetRole operation: The role cannot be found" >&2; exit 254`

// A hub role that does not exist stops the phase before anything is installed for it:
// provider-opentofu would report Healthy with no credentials.
func TestVerifyFleetHubRole_AMissingRoleRefusesTheApply(t *testing.T) {
	fakeIAM(t, noSuchRole)
	st := fleetState(t, providersFixture)
	st.Config.Cloud.AccountID = "111111111111"
	err := verifyFleetHubRole(context.Background(), st)
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("a missing role must fail the phase, got %v", err)
	}
}

// A plan reads for real and only reports: on a first install the role is created by the
// substrate phase that the plan has not run.
func TestVerifyFleetHubRole_APlanReportsAMissingRole(t *testing.T) {
	log := fakeIAM(t, noSuchRole)
	st := fleetState(t, providersFixture)
	st.Config.Cloud.AccountID = "111111111111"
	st.Runner.DryRun = true
	var out strings.Builder
	st.Runner.Out = &out
	if err := verifyFleetHubRole(context.Background(), st); err != nil {
		t.Fatalf("a plan reports, it does not fail: %v", err)
	}
	if b, _ := os.ReadFile(log); !strings.Contains(string(b), "iam get-role --role-name eks-fleet-crossplane") {
		t.Errorf("the plan must ask IAM for real:\n%s", b)
	}
}

func TestVerifyFleetHubRole_TheRoleThatExistsPasses(t *testing.T) {
	fakeIAM(t, "echo arn:aws:iam::111111111111:role/eks-fleet-crossplane")
	st := fleetState(t, providersFixture)
	st.Config.Cloud.AccountID = "111111111111"
	if err := verifyFleetHubRole(context.Background(), st); err != nil {
		t.Fatalf("the role exists: %v", err)
	}
}

// A role found under another path is not the ARN the annotation would carry.
func TestVerifyFleetHubRole_ADifferentARNRefuses(t *testing.T) {
	fakeIAM(t, "echo arn:aws:iam::111111111111:role/fleet/eks-fleet-crossplane")
	st := fleetState(t, providersFixture)
	st.Config.Cloud.AccountID = "111111111111"
	if err := verifyFleetHubRole(context.Background(), st); err == nil || !strings.Contains(err.Error(), "role/fleet/") {
		t.Fatalf("a different ARN must refuse, naming the real one; got %v", err)
	}
}

// A role in another account cannot be read from here, and a read that did not answer says
// nothing about the role: both are reported unverified, and neither refuses.
func TestVerifyFleetHubRole_UnverifiableIsReportedNotRefused(t *testing.T) {
	log := fakeIAM(t, `echo "An error occurred (AccessDenied)" >&2; exit 254`)
	st := fleetState(t, providersFixture)
	var out strings.Builder
	st.Runner.Out = &out
	st.Config.Cloud.AccountID = "222222222222"
	if err := verifyFleetHubRole(context.Background(), st); err != nil {
		t.Fatalf("a cross-account role is unverified, not refused: %v", err)
	}
	if b, _ := os.ReadFile(log); len(b) != 0 {
		t.Errorf("a role in another account is not asked about:\n%s", b)
	}
	st.Config.Cloud.AccountID = "111111111111"
	st.Runner.ReadAttempts = 1
	if err := verifyFleetHubRole(context.Background(), st); err != nil {
		t.Fatalf("an unanswered read is unverified, not refused: %v", err)
	}
	if strings.Count(out.String(), "UNVERIFIED") != 2 {
		t.Errorf("both must say UNVERIFIED:\n%s", out.String())
	}
	st.Config.ControlPlane.FleetHubRoleARN = "not-an-arn"
	if err := verifyFleetHubRole(context.Background(), st); err == nil {
		t.Error("a value that is not an ARN cannot be verified")
	}
}

// Through the phase: a missing hub role installs nothing at all.
func TestFleet_AMissingHubRoleInstallsNothing(t *testing.T) {
	st, calls := fakeTools(t)
	fakeIAM(t, noSuchRole)
	st.Config.ControlPlane.EKSFleet = true
	st.Config.ControlPlane.FleetHubRoleARN = "arn:aws:iam::111111111111:role/eks-fleet-crossplane"
	st.Config.Cloud.AccountID = "111111111111"
	st.Repos.EKSFleet = t.TempDir()
	if err := (fleet{}).Run(context.Background(), st); err == nil {
		t.Fatal("a missing hub role must fail the phase")
	}
	if got := calls(); len(got) != 0 {
		t.Errorf("nothing may be installed for a role that does not exist:\n%v", got)
	}
}
