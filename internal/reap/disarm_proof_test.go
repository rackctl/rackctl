package reap

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/exec"
)

// silentKubectl puts a kubectl on PATH that exits 0 and prints nothing — the shape of a
// shimmed binary, an API answering with no body, or a query whose shape stopped matching.
func silentKubectl(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write silent kubectl: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// The disarm's tick is a safety claim: selfHeal is off, so the Platform CRs the reap is
// about to delete will stay deleted. A kubectl that answers nothing supplies no evidence
// for that claim, and the reap's own failure path spells out what riding on it costs —
// recreated CRs, orphaned IAM roles, agent-iam failing later on DeleteConflict.
func TestDisarmWithholdsItsTickWhenTheApplicationsCannotBeRead(t *testing.T) {
	silentKubectl(t)
	var buf bytes.Buffer

	disarmArgoCD(context.Background(), exec.New(&buf), &buf)

	if strings.Contains(buf.String(), "ArgoCD is passive") {
		t.Errorf("a silent kubectl certified the disarm:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "could not confirm") {
		t.Errorf("an unverifiable disarm must say so rather than pass quietly:\n%s", buf.String())
	}
}

// An empty Application list that actually parsed is a real answer, and the tick belongs to
// it. Withholding the tick here would make the check unable to ever report success.
func TestDisarmTicksWhenTheApplicationListParsesEmpty(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
case "$*" in
  *"get applications -o jsonpath"*) ;;
  *"get applications -o json"*) echo '{"items":[]}' ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake kubectl: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var buf bytes.Buffer
	disarmArgoCD(context.Background(), exec.New(&buf), &buf)

	if !strings.Contains(buf.String(), "ArgoCD is passive") {
		t.Errorf("a parsed empty list is a real answer and earns the tick:\n%s", buf.String())
	}
}
