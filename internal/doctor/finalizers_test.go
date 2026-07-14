package doctor

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/exec"
)

// fakeKubectl puts a `kubectl` on PATH that answers `get <kind> -A -o json` from a table
// of canned responses, keyed by the kind. Anything not in the table returns an error, so
// an uninstalled CRD is exercised too — the agent platform is optional and its absence
// must not be a failure.
//
// The Runner shells out by name, so this is the seam: the test exercises the real
// argument-building and JSON decoding, not a stub of them.
func fakeKubectl(t *testing.T, byKind map[string]string) *Env {
	t.Helper()
	dir := t.TempDir()

	var cases strings.Builder
	for kind, body := range byKind {
		fmt.Fprintf(&cases, "  %q) cat <<'JSON'\n%s\nJSON\n  ;;\n", kind, body)
	}
	script := fmt.Sprintf(`#!/bin/sh
# args: get <kind> -A -o json
case "$2" in
%s  *) exit 1 ;;
esac
exit 0
`, cases.String())

	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake kubectl: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return &Env{Cfg: &config.Config{}, Run: exec.New(io.Discard)}
}

func terminatingSince(d time.Duration) string {
	return time.Now().Add(-d).UTC().Format(time.RFC3339)
}

// The Platform wedge: its finalizer called PutBucketPolicy with an empty statement list,
// S3 rejected it (`MalformedPolicy: Statement is empty!`), and it retried forever. ArgoCD
// rates a resource pending deletion as Progressing, so the agent-platform Application
// never went Healthy and the convergence gate could never pass. Nothing in doctor saw it.
func TestCheckStuckFinalizers_CatchesAPlatformWedgedInTerminating(t *testing.T) {
	env := fakeKubectl(t, map[string]string{
		"platforms.platform.nanohype.dev": fmt.Sprintf(`{"items":[{"metadata":{
			"name":"ops","namespace":"eks-agent-platform",
			"deletionTimestamp":%q,
			"finalizers":["platform.nanohype.dev/platform-finalizer"]}}]}`, terminatingSince(90*time.Minute)),
	})

	r := CheckStuckFinalizers(context.Background(), env)
	if r.Status != Fail {
		t.Fatalf("a Platform stuck Terminating for 90m must FAIL — it holds its Application at "+
			"Progressing forever and the convergence gate can never pass; got %s: %s", r.Status, r.Detail)
	}
	for _, want := range []string{"ops", "platform-finalizer", "Progressing"} {
		if !strings.Contains(r.Detail, want) {
			t.Errorf("detail must name %q so the wedge is diagnosable:\n%s", want, r.Detail)
		}
	}
}

// The PVC wedge: kubernetes.io/pvc-protection could not clear because ArgoCD's selfHeal
// kept re-creating the pod that mounted it. Same consequence, different object.
func TestCheckStuckFinalizers_CatchesAPVCWedgedOnPVCProtection(t *testing.T) {
	env := fakeKubectl(t, map[string]string{
		"pvc": fmt.Sprintf(`{"items":[{"metadata":{
			"name":"kagent-postgresql","namespace":"kagent",
			"deletionTimestamp":%q,
			"finalizers":["kubernetes.io/pvc-protection"]}}]}`, terminatingSince(2*time.Hour)),
	})

	r := CheckStuckFinalizers(context.Background(), env)
	if r.Status != Fail {
		t.Fatalf("a PVC stuck on pvc-protection must FAIL; got %s: %s", r.Status, r.Detail)
	}
	if !strings.Contains(r.Detail, "kagent/kagent-postgresql") {
		t.Errorf("detail must name the namespaced object:\n%s", r.Detail)
	}
}

// A deletion that is merely IN PROGRESS is not a wedge. Without this, every teardown and
// every routine prune would trip the check, and a doctor that cries wolf is a doctor
// nobody reads.
func TestCheckStuckFinalizers_IgnoresARecentDeletion(t *testing.T) {
	env := fakeKubectl(t, map[string]string{
		"pvc": fmt.Sprintf(`{"items":[{"metadata":{
			"name":"scratch","namespace":"default",
			"deletionTimestamp":%q,
			"finalizers":["kubernetes.io/pvc-protection"]}}]}`, terminatingSince(30*time.Second)),
	})

	if r := CheckStuckFinalizers(context.Background(), env); r.Status != OK {
		t.Fatalf("a deletion 30s old is in progress, not wedged; got %s: %s", r.Status, r.Detail)
	}
}

// Objects that are not being deleted at all must not be flagged.
func TestCheckStuckFinalizers_IgnoresLiveObjects(t *testing.T) {
	env := fakeKubectl(t, map[string]string{
		"platforms.platform.nanohype.dev": `{"items":[{"metadata":{
			"name":"ops","namespace":"eks-agent-platform",
			"finalizers":["platform.nanohype.dev/platform-finalizer"]}}]}`,
	})

	if r := CheckStuckFinalizers(context.Background(), env); r.Status != OK {
		t.Fatalf("a live Platform carrying a finalizer is normal, not stuck; got %s: %s", r.Status, r.Detail)
	}
}

// An uninstalled CRD is not a failure — the agent platform is optional.
func TestCheckStuckFinalizers_ToleratesMissingCRDs(t *testing.T) {
	env := fakeKubectl(t, map[string]string{}) // every kind errors

	if r := CheckStuckFinalizers(context.Background(), env); r.Status != OK {
		t.Fatalf("a cluster without the platform CRDs must pass; got %s: %s", r.Status, r.Detail)
	}
}
