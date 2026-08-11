package doctor

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/exec"
)

// fakeApplications puts a kubectl on PATH answering the Application list call.
//
// CheckApplications invokes `kubectl -n argocd get applications -o json`, so the argument
// shape differs from the cluster-scoped `get <kind> -A` the finalizer tests fake. Matching
// on the whole argument string keeps this honest about which call it is answering.
func fakeApplications(t *testing.T, body string) *Env {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
case "$*" in
  *'get applications'*) cat <<'JSON'
` + body + `
JSON
  ;;
  *) exit 1 ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake kubectl: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return &Env{Cfg: &config.Config{}, Run: exec.New(io.Discard)}
}

// appsJSON builds an Application list from name=health/sync triples.
func appsJSON(entries ...string) string {
	var b strings.Builder
	b.WriteString(`{"items":[`)
	for i, e := range entries {
		name, rest, _ := strings.Cut(e, "=")
		health, sync, _ := strings.Cut(rest, "/")
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"metadata":{"name":"` + name + `"},"status":{"health":{"status":"` +
			health + `"},"sync":{"status":"` + sync + `"}}}`)
	}
	b.WriteString(`]}`)
	return b.String()
}

// Health is the assertion, and it has to stay one: a Degraded or Missing Application is a
// workload that is not running, whatever its sync status says.
func TestCheckApplications_FailsOnUnhealthy(t *testing.T) {
	env := fakeApplications(t, appsJSON("cilium=Healthy/Synced", "tempo=Progressing/Synced"))

	r := CheckApplications(context.Background(), env)
	if r.Status != Fail {
		t.Fatalf("an Application that is not Healthy must fail: %s — %s", r.Status, r.Detail)
	}
	if !strings.Contains(r.Detail, "tempo") {
		t.Errorf("the detail must name the offender:\n%s", r.Detail)
	}
}

// And the change: Healthy but OutOfSync is reported, not failed.
//
// Demanding Synced made this check unable to pass on a working cluster. Charts that
// regenerate a self-signed certificate on every render diff forever, CRDs pick up
// server-side defaults, and a CR its own operator writes back to differs the moment it is
// reconciled. A check that cannot pass is one operators learn to skip.
func TestCheckApplications_WarnsButDoesNotFailOnOutOfSync(t *testing.T) {
	env := fakeApplications(t, appsJSON("cilium=Healthy/OutOfSync", "loki=Healthy/Synced"))

	r := CheckApplications(context.Background(), env)
	if r.Status == Fail {
		t.Fatalf("Healthy/OutOfSync must not fail the check — it is the steady state of several "+
			"addons: %s", r.Detail)
	}
	if r.Status != Warn {
		t.Fatalf("it must still be reported, not swallowed: %s — %s", r.Status, r.Detail)
	}
	if !strings.Contains(r.Detail, "cilium") {
		t.Errorf("the warning must name which Applications are OutOfSync:\n%s", r.Detail)
	}
}

func TestCheckApplications_OKWhenAllHealthyAndSynced(t *testing.T) {
	env := fakeApplications(t, appsJSON("cilium=Healthy/Synced", "loki=Healthy/Synced"))

	if r := CheckApplications(context.Background(), env); r.Status != OK {
		t.Fatalf("a fully converged catalog must pass: %s — %s", r.Status, r.Detail)
	}
}

// An empty catalog is the vacuous-pass signature this whole family of checks exists to
// refuse: app-of-apps generated nothing and every loop over it trivially succeeds.
func TestCheckApplications_FailsOnAnEmptyCatalog(t *testing.T) {
	env := fakeApplications(t, `{"items":[]}`)

	if r := CheckApplications(context.Background(), env); r.Status != Fail {
		t.Fatalf("no Applications must fail: %s — %s", r.Status, r.Detail)
	}
}
