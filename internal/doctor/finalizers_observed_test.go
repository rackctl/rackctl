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

func kubectlReturning(t *testing.T, body string) *Env {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write fake kubectl: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return &Env{Cfg: config.Default(), Run: exec.New(io.Discard)}
}

// Skipping a kind whose CRD is absent is correct; skipping every kind is a different
// answer. `pvc` and `namespaces` are core, so reading none of them means the API was never
// read — and "nothing wedged" would rest on zero observations.
func TestStuckFinalizersWithholdsOKWhenNoKindWasRead(t *testing.T) {
	r := CheckStuckFinalizers(context.Background(), kubectlReturning(t, "exit 0\n"))

	if r.Status == OK {
		t.Errorf("a kubectl that reads nothing certified the cluster: %s", r.Detail)
	}
	if !strings.Contains(r.Detail, "could not read") {
		t.Errorf("the verdict must say the kinds were unreadable:\n%s", r.Detail)
	}
}

// The optional CRDs may genuinely be absent while the core kinds read fine. That is a real
// observation of an empty result and keeps its tick — a check corrected until it can never
// pass is its own defect.
func TestStuckFinalizersOKWhenCoreKindsReadEmpty(t *testing.T) {
	env := kubectlReturning(t, `
case "$*" in
  *"get pvc"*|*"get namespaces"*) echo '{"items":[]}' ;;
  *) exit 1 ;;
esac
exit 0
`)

	if r := CheckStuckFinalizers(context.Background(), env); r.Status != OK {
		t.Errorf("core kinds reading an empty list is a real answer: %s — %s", r.Status, r.Detail)
	}
}
