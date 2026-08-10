package phases

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/engine"
	"github.com/rackctl/rackctl/internal/exec"
)

// portalCRState writes a portal checkout containing platform.yaml and puts a fake kubectl
// on PATH that logs every invocation. `saExists` decides whether `get serviceaccount
// tenant-runtime` succeeds, which is the condition the wait turns on.
func portalCRState(t *testing.T, saExists bool) (*engine.State, string) {
	t.Helper()
	dir := t.TempDir()
	checkout := filepath.Join(dir, "portal")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "platform.yaml"), []byte("kind: Platform\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	logf := filepath.Join(dir, "invocations")
	sa := "exit 1"
	if saExists {
		sa = "echo serviceaccount/tenant-runtime; exit 0"
	}
	script := "#!/bin/sh\necho \"$@\" >> " + logf + "\n" +
		"case \"$*\" in\n" +
		"  *'get serviceaccount tenant-runtime'*) " + sa + " ;;\n" +
		"  *'get platform portal'*) echo 'Pending Ready=False(AwaitingNamespace)'; exit 0 ;;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return &engine.State{
		Config: config.Default(),
		Runner: exec.New(io.Discard), // NOT dry-run — the point is to exec
		Repos:  engine.Repos{Portal: checkout},
	}, logf
}

// The gap this closes: rackctl read portal/platform.yaml to build TF_VAR_tenants and never
// applied it. Everything the chart needs — the namespace, the tenant-runtime ServiceAccount,
// the Pod Identity association — is reconciled by the operator FROM that object, so without
// the apply the pods reference an account that does not exist and never schedule.
func TestApplyPortalPlatform_AppliesTheCR(t *testing.T) {
	st, logf := portalCRState(t, true)

	if err := applyPortalPlatform(context.Background(), st, "tenants-portal"); err != nil {
		t.Fatalf("applyPortalPlatform: %v", err)
	}
	b, err := os.ReadFile(logf)
	if err != nil {
		t.Fatalf("kubectl was never invoked at all: %v", err)
	}
	got := string(b)
	if !strings.Contains(got, "apply -f") || !strings.Contains(got, "platform.yaml") {
		t.Fatalf("portal's Platform CR must be applied before the chart:\n%s", got)
	}
}

// Waiting on the ServiceAccount rather than on Platform.status.phase is deliberate: the
// phase is the operator's summary of its own work, the account is what a pod's admission
// resolves. A timeout here must fail the phase — installing anyway produces Deployments
// whose pods stay unschedulable while helm reports the release deployed.
func TestApplyPortalPlatform_FailsWhenTheServiceAccountNeverAppears(t *testing.T) {
	st, _ := portalCRState(t, false)

	done := make(chan error, 1)
	go func() {
		done <- waitTenantServiceAccount(context.Background(), st, "tenants-portal", tenantRuntimeSA, 1*time.Second)
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a missing tenant-runtime must fail the phase — portal's pods run as it")
		}
		// The message has to carry the operator's own verdict, or the operator is left
		// diagnosing an absence.
		if !strings.Contains(err.Error(), "Platform status") {
			t.Errorf("the failure must report what the operator thinks:\n%v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the wait did not honour its timeout")
	}
}

// readyState puts a fake kubectl on PATH: `get deploy -o name` answers `deploys`, and
// `rollout status` succeeds or fails per `rolloutOK`.
func readyState(t *testing.T, deploys string, rolloutOK bool) *engine.State {
	t.Helper()
	dir := t.TempDir()
	rollout := "exit 1"
	if rolloutOK {
		rollout = "exit 0"
	}
	script := "#!/bin/sh\ncase \"$*\" in\n" +
		"  *'get deploy'*) printf '" + deploys + "' ;;\n" +
		"  *'rollout status'*) " + rollout + " ;;\n" +
		"  *'get pods'*) echo 'portal-server-x Pending Unschedulable error looking up service account' ;;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return &engine.State{Config: config.Default(), Runner: exec.New(io.Discard)}
}

func TestWaitPortalReady_PassesWhenEveryDeploymentRollsOut(t *testing.T) {
	st := readyState(t, "deployment.apps/portal-server\\ndeployment.apps/portal-web\\n", true)

	if err := waitPortalReady(context.Background(), st, "tenants-portal", 30*time.Second); err != nil {
		t.Fatalf("a release whose deployments roll out is ready: %v", err)
	}
}

// The defect this exists to refuse. `kubectl wait -l <selector>` matching nothing exits 0,
// which is exactly how the catalog-convergence gate passed vacuously on every install it
// ever ran. A release that rendered no Deployment is a failure, not a fast success.
func TestWaitPortalReady_RefusesAnEmptySet(t *testing.T) {
	st := readyState(t, "", true)

	err := waitPortalReady(context.Background(), st, "tenants-portal", 30*time.Second)
	if err == nil {
		t.Fatal("no Deployments must fail — matching nothing is the vacuous pass this replaces")
	}
	if !strings.Contains(err.Error(), "no portal Deployment") {
		t.Errorf("the message must say the set was empty:\n%v", err)
	}
}

// A rollout timeout names the Deployment and nothing about why. The two causes that matter
// — an absent ServiceAccount and a container that cannot start — are both one level down.
func TestWaitPortalReady_ReportsWhyThePodsAreNotRunning(t *testing.T) {
	st := readyState(t, "deployment.apps/portal-server\\n", false)

	err := waitPortalReady(context.Background(), st, "tenants-portal", 30*time.Second)
	if err == nil {
		t.Fatal("a deployment that never becomes available must fail the phase")
	}
	if !strings.Contains(err.Error(), "service account") {
		t.Errorf("the failure must carry the pod-level reason, not just the rollout timeout:\n%v", err)
	}
}

// The namespace portal's Platform and BudgetPolicy live in is not created by anything on a
// fresh cluster. Every other tenant app gets theirs from ArgoCD (CreateNamespace=true on
// its Application); portal is installed by this tool, so nothing does — and `kubectl apply`
// into a namespace that does not exist fails outright.
func TestEnsureNamespace_CreatesItWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	logf := filepath.Join(dir, "invocations")
	// `get namespace` fails (absent); everything else succeeds.
	script := "#!/bin/sh\necho \"$@\" >> " + logf + "\n" +
		"case \"$*\" in *'get namespace'*) exit 1 ;; esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	st := &engine.State{Config: config.Default(), Runner: exec.New(io.Discard)}

	if err := ensureNamespace(context.Background(), st, portalControlPlaneNS); err != nil {
		t.Fatalf("ensureNamespace: %v", err)
	}
	b, _ := os.ReadFile(logf)
	if !strings.Contains(string(b), "create namespace "+portalControlPlaneNS) {
		t.Fatalf("an absent namespace must be created:\n%s", b)
	}
}

// And it must not fight a namespace that already exists — the common case once a cluster
// has been installed before.
func TestEnsureNamespace_NoOpWhenPresent(t *testing.T) {
	dir := t.TempDir()
	logf := filepath.Join(dir, "invocations")
	script := "#!/bin/sh\necho \"$@\" >> " + logf + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	st := &engine.State{Config: config.Default(), Runner: exec.New(io.Discard)}

	if err := ensureNamespace(context.Background(), st, portalControlPlaneNS); err != nil {
		t.Fatalf("ensureNamespace: %v", err)
	}
	b, _ := os.ReadFile(logf)
	if strings.Contains(string(b), "create namespace") {
		t.Fatalf("an existing namespace must not be recreated:\n%s", b)
	}
}
