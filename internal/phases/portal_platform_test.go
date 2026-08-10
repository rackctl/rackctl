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
