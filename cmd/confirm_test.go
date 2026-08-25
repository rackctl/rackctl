package cmd

import (
	"os"
	"strings"
	"testing"
)

// withStdin points os.Stdin at a pipe carrying s, which is what fmt.Scanln reads. Closing
// the write end without writing produces EOF — the "nobody is there" case.
func withStdin(t *testing.T, s string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old; _ = r.Close() })

	if s != "" {
		if _, err := w.WriteString(s); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	_ = w.Close()
}

// The whole point of the gate: the exact cluster name, and nothing else, proceeds.
func TestConfirmDestroy_ExactNameProceeds(t *testing.T) {
	destroyYes = false
	t.Cleanup(func() { destroyYes = false })
	withStdin(t, "development-platform\n")

	if err := confirmDestroy("development-platform", "development"); err != nil {
		t.Fatalf("the typed name matched and must be accepted: %v", err)
	}
}

// A near-miss is the failure this gate exists for — tearing down the environment you meant
// to keep. Anything that is not the name refuses.
func TestConfirmDestroy_WrongNameRefuses(t *testing.T) {
	destroyYes = false
	t.Cleanup(func() { destroyYes = false })

	for _, typed := range []string{"staging-platform\n", "y\n", "yes\n", "development\n"} {
		withStdin(t, typed)
		err := confirmDestroy("development-platform", "development")
		if err == nil {
			t.Fatalf("%q is not the cluster name and must not authorise a teardown", strings.TrimSpace(typed))
		}
		if !strings.Contains(err.Error(), "nothing was destroyed") {
			t.Errorf("a refusal must say nothing was destroyed.\ngot: %v", err)
		}
	}
}

// EOF means there is nobody to type, which is a refusal with its own reason.
//
// An os.Stdin.Stat() ModeCharDevice test would pass here — /dev/null is a character device —
// and the operator would be told their confirmation did not match a question nobody asked. A
// misleading reason on a refusal is how a guard gets worked around instead of understood.
func TestConfirmDestroy_EOFRefusesWithItsOwnReason(t *testing.T) {
	destroyYes = false
	t.Cleanup(func() { destroyYes = false })
	withStdin(t, "")

	err := confirmDestroy("development-platform", "development")
	if err == nil {
		t.Fatal("a teardown that cannot ask is a teardown nobody authorised")
	}
	if !strings.Contains(err.Error(), "nobody to confirm") {
		t.Errorf("EOF must refuse for the EOF reason, not as a name mismatch.\ngot: %v", err)
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Errorf("the refusal must name the flag a scripted teardown needs.\ngot: %v", err)
	}
}

// --yes is the unattended path: it must not read stdin at all, so a CI runner with no
// stdin still tears down.
func TestConfirmDestroy_YesSkipsThePrompt(t *testing.T) {
	destroyYes = true
	t.Cleanup(func() { destroyYes = false })
	withStdin(t, "")

	if err := confirmDestroy("development-platform", "development"); err != nil {
		t.Fatalf("--yes must proceed without asking: %v", err)
	}
}

// Surrounding whitespace is a typing artifact, not a different answer.
func TestConfirmDestroy_TrimsTheTypedName(t *testing.T) {
	destroyYes = false
	t.Cleanup(func() { destroyYes = false })
	withStdin(t, "  development-platform  \n")

	if err := confirmDestroy("development-platform", "development"); err != nil {
		t.Fatalf("a name with surrounding whitespace is the same name: %v", err)
	}
}
