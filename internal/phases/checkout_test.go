package phases

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/engine"
	rexec "github.com/rackctl/rackctl/internal/exec"
)

// git runs a command in dir and fails the test if it errors. Real git rather than a
// fake runner: every behaviour under test here — what --force does to a dirty tree,
// what --ff-only does on a detached HEAD — is git's, and a mock would only assert
// that we believe our own description of it.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// originAndClone builds an upstream repo with a tag and a later commit on the
// default branch, then clones it. Returns the clone's path.
func originAndClone(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, origin, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(origin, "leaf.hcl"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, origin, "add", "leaf.hcl")
	git(t, origin, "commit", "-qm", "v1")
	git(t, origin, "tag", "deploy-v1.0.0")
	if err := os.WriteFile(filepath.Join(origin, "leaf.hcl"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, origin, "commit", "-qam", "v2")

	clone := filepath.Join(root, "clone")
	git(t, root, "clone", "-q", origin, clone)
	return clone
}

func state(dir string) *engine.State {
	r := rexec.New(io.Discard)
	r.Dir = dir
	return &engine.State{Runner: r}
}

// A pinned run must not throw away uncommitted operator edits. The documented
// recovery from a failed apply is to edit the leaf and re-run — and `git checkout
// --force` deleted the edit, then re-applied the pinned tree, so the operator
// watched the same failure recur with no sign their fix was gone.
func TestPinnedCheckoutRefusesToDiscardLocalEdits(t *testing.T) {
	clone := originAndClone(t)
	edit := filepath.Join(clone, "leaf.hcl")
	if err := os.WriteFile(edit, []byte("operator's unblocking fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := cloneOrUpdate(context.Background(), state(clone), "", clone, "deploy-v1.0.0")

	if err == nil {
		t.Fatal("a pinned checkout over uncommitted edits must refuse, not proceed")
	}
	var noRollback *engine.NoRollbackError
	if !errors.As(err, &noRollback) {
		t.Errorf("err = %T, want NoRollbackError — a dirty checkout is not a reason to tear down cloud", err)
	}
	if got, _ := os.ReadFile(edit); string(got) != "operator's unblocking fix\n" {
		t.Errorf("the operator's edit was destroyed: %q", got)
	}
	if !strings.Contains(err.Error(), "leaf.hcl") {
		t.Errorf("the refusal must name the dirty files, got: %v", err)
	}
}

// The same checkout with a clean tree still pins, or the guard above would just be
// a way to break pinning.
func TestPinnedCheckoutProceedsOnACleanTree(t *testing.T) {
	clone := originAndClone(t)

	if err := cloneOrUpdate(context.Background(), state(clone), "", clone, "deploy-v1.0.0"); err != nil {
		t.Fatalf("cloneOrUpdate on a clean tree: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(clone, "leaf.hcl")); string(got) != "v1\n" {
		t.Errorf("checkout did not land on the pinned tag; leaf.hcl = %q, want %q", got, "v1\n")
	}
}

// Removing a pin has to actually unpin. A previous pinned run leaves the checkout on
// a detached HEAD, and `git pull --ff-only` cannot fast-forward one — so the repo
// stayed on the old commit while the note blamed a divergence that never happened.
func TestUnpinningReturnsToTheDefaultBranch(t *testing.T) {
	clone := originAndClone(t)
	ctx := context.Background()

	// Pin it, as a previous run would have.
	if err := cloneOrUpdate(ctx, state(clone), "", clone, "deploy-v1.0.0"); err != nil {
		t.Fatalf("pinning: %v", err)
	}
	if head := git(t, clone, "rev-parse", "--abbrev-ref", "HEAD"); head != "HEAD" {
		t.Fatalf("fixture is not on a detached HEAD after pinning, got %q", head)
	}

	// Now the pin is removed from versions.
	if err := cloneOrUpdate(ctx, state(clone), "", clone, ""); err != nil {
		t.Fatalf("unpinning: %v", err)
	}

	if head := git(t, clone, "rev-parse", "--abbrev-ref", "HEAD"); head != "main" {
		t.Errorf("HEAD = %q after unpinning, want the default branch — removing the pin left it detached", head)
	}
	if got, _ := os.ReadFile(filepath.Join(clone, "leaf.hcl")); string(got) != "v2\n" {
		t.Errorf("unpinned checkout is still on the pinned commit; leaf.hcl = %q, want %q", got, "v2\n")
	}
}

// Unpinning an already-attached checkout is the common case and must stay a plain
// fast-forward.
func TestUnpinningAnAttachedCheckoutStillFastForwards(t *testing.T) {
	clone := originAndClone(t)
	git(t, clone, "reset", "-q", "--hard", "HEAD~1") // behind, but on main

	if err := cloneOrUpdate(context.Background(), state(clone), "", clone, ""); err != nil {
		t.Fatalf("cloneOrUpdate: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(clone, "leaf.hcl")); string(got) != "v2\n" {
		t.Errorf("leaf.hcl = %q, want %q — the checkout did not fast-forward", got, "v2\n")
	}
}
