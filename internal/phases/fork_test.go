package phases

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/engine"
	"github.com/rackctl/rackctl/internal/exec"
)

// fakeGH puts a `gh` on PATH that records every invocation and reports whether the
// fork exists via the exit code of `gh repo view` — which is exactly how forkOrSync
// decides. Everything else succeeds.
//
// The Runner shells out by name, so this is the seam: no interface to stub, and the
// real behaviour (which subcommands are issued, with which flags) is what we care
// about pinning.
func fakeGH(t *testing.T, forkExists bool) func() []string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")

	viewExit := 1 // `gh repo view` fails ⇒ no fork
	if forkExists {
		viewExit = 0
	}

	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
if [ "$1" = "repo" ] && [ "$2" = "view" ]; then exit %d; fi
exit 0
`, logPath, viewExit)

	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return func() []string {
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

func newState() *engine.State {
	r := exec.New(io.Discard)
	return &engine.State{Runner: r}
}

// An existing fork must be SYNCED, not merely reused.
//
// This is the bug this test exists for: forkIfMissing saw the fork, said "reusing the
// fork", and moved on — leaving it at whatever commit it was forked at. The cluster
// reads its catalog from the fork, never from upstream, so a fix merged upstream is
// simply absent from the cluster built afterwards. Nothing errors; the catalog is
// valid, just old. It is the same "present is not current" bug cloneOrUpdate fixed for
// local checkouts, and it silently disproves the very fix a run is meant to prove.
func TestForkOrSync_ExistingForkIsBroughtUpToDate(t *testing.T) {
	calls := fakeGH(t, true)

	if err := forkOrSync(context.Background(), newState(), "acme"); err != nil {
		t.Fatalf("forkOrSync: %v", err)
	}

	got := calls()
	if !slicesContainsFunc(got, func(c string) bool {
		return strings.HasPrefix(c, "repo sync acme/eks-gitops")
	}) {
		t.Fatalf("an existing fork was not synced with upstream.\ncalls: %#v", got)
	}
	if slicesContainsFunc(got, func(c string) bool { return strings.HasPrefix(c, "repo fork") }) {
		t.Errorf("re-forked a fork that already exists.\ncalls: %#v", got)
	}
}

// The sync must never hard-reset the fork.
//
// The org OWNS this fork and is expected to commit to it — that is the entire reason
// the catalog is forked rather than consumed. `gh repo sync --force` hard-resets the
// destination, which would silently destroy the operator's own commits. Fast-forward
// or report; never overwrite.
func TestForkOrSync_NeverForceOverwritesTheOrgsCommits(t *testing.T) {
	calls := fakeGH(t, true)

	if err := forkOrSync(context.Background(), newState(), "acme"); err != nil {
		t.Fatalf("forkOrSync: %v", err)
	}

	for _, c := range calls() {
		if strings.Contains(c, "--force") {
			t.Fatalf("sync passed --force, which hard-resets the fork and would destroy "+
				"commits the org owns: %q", c)
		}
	}
}

// A missing fork is still forked — the original behaviour must survive the change.
func TestForkOrSync_MissingForkIsCreated(t *testing.T) {
	calls := fakeGH(t, false)

	if err := forkOrSync(context.Background(), newState(), "acme"); err != nil {
		t.Fatalf("forkOrSync: %v", err)
	}

	got := calls()
	if !slicesContainsFunc(got, func(c string) bool {
		return strings.HasPrefix(c, "repo fork nanohype/eks-gitops")
	}) {
		t.Fatalf("a missing fork was not created.\ncalls: %#v", got)
	}
}

// A fork that has diverged is reported, not fatal: the run continues against the
// catalog as it stands. Divergence is legitimate — the org owns the fork.
func TestForkOrSync_DivergedForkIsNotFatal(t *testing.T) {
	dir := t.TempDir()
	// `gh repo view` succeeds (fork exists); `gh repo sync` fails (diverged).
	script := `#!/bin/sh
if [ "$1" = "repo" ] && [ "$2" = "sync" ]; then exit 1; fi
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := forkOrSync(context.Background(), newState(), "acme"); err != nil {
		t.Fatalf("a diverged fork must not fail the run — the org is entitled to its own "+
			"commits; got: %v", err)
	}
}

func slicesContainsFunc(s []string, f func(string) bool) bool {
	for _, v := range s {
		if f(v) {
			return true
		}
	}
	return false
}
