package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// mustWrite records output and fails the test if the write itself did.
//
// Not `_ =`: a test that ignores an error is how a test passes while the thing it names is
// broken, which is why .golangci.yml deliberately does not exclude test files from
// errcheck.
func mustWrite(t *testing.T, tr *transcript, s string) {
	t.Helper()
	if _, err := tr.Write([]byte(s)); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// Subprocess output must reach a file. Discarding it leaves a failed forty-minute install
// with a cross beside a phase name and nothing else — and that output is the diagnosis.
func TestTranscript_RecordsWhatWasWritten(t *testing.T) {
	dir := t.TempDir()
	tr := newTranscript(dir, "apply")
	defer tr.Close()

	if _, err := tr.Write([]byte("terragrunt: applying network\nError: DependencyViolation\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	tr.Close()

	if tr.Path() == "" {
		t.Fatal("no transcript was opened")
	}
	got, err := os.ReadFile(tr.Path())
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(string(got), "DependencyViolation") {
		t.Errorf("the transcript lost the output it exists to keep:\n%s", got)
	}
	if filepath.Dir(tr.Path()) != dir {
		t.Errorf("transcript landed at %s, want a file under %s", tr.Path(), dir)
	}
}

// The tail is what the view renders under the running phase, so a long wait shows what it
// is waiting on rather than a spinner alone. It is the most recent NON-EMPTY line: trailing
// newlines are the common case and would otherwise blank it.
func TestTranscript_TailIsTheLastNonEmptyLine(t *testing.T) {
	tr := newTranscript(t.TempDir(), "apply")
	defer tr.Close()

	mustWrite(t, tr, "first\n")
	mustWrite(t, tr, "waiting for the EKS control plane\n\n")

	if got := tr.Tail(0); got != "waiting for the EKS control plane" {
		t.Fatalf("tail = %q", got)
	}
}

// A tail wider than the terminal would wrap and shift the whole view, so it is truncated
// with a marker rather than cut silently.
func TestTranscript_TailIsTruncatedToWidth(t *testing.T) {
	tr := newTranscript(t.TempDir(), "apply")
	defer tr.Close()
	// Multi-byte on purpose: a byte-sliced truncation cuts one of these in half and the
	// result is not valid UTF-8, which the terminal renders as a replacement character.
	mustWrite(t, tr, strings.Repeat("é", 200)+"\n")

	// Measured in runes, because that is what a terminal column is. A byte length would
	// pass here for an ASCII line and hide the overshoot on any line that is not.
	got := tr.Tail(40)
	if n := len([]rune(got)); n > 40 {
		t.Fatalf("tail is %d runes, want at most 40", n)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("a truncated tail must say so, got %q", got)
	}
	if !utf8.ValidString(got) {
		t.Errorf("truncation split a multi-byte character: %q", got)
	}
}

// A transcript that cannot be opened must not fail the run. It is a diagnostic aid, and
// refusing to provision because a log file could not be created makes the aid more
// important than the thing it documents — so writes still succeed and the report says
// plainly that nothing was recorded.
func TestTranscript_UnopenableIsNotFatalAndSaysSo(t *testing.T) {
	tr := newTranscript("", "apply")
	defer tr.Close()

	if _, err := tr.Write([]byte("still accepted\n")); err != nil {
		t.Fatalf("writes must still succeed with no file: %v", err)
	}
	if tr.Path() != "" {
		t.Errorf("no file should have been opened, got %q", tr.Path())
	}
	if got := tr.Tail(0); got != "still accepted" {
		t.Errorf("the in-memory tail must still work, got %q", got)
	}

	var b strings.Builder
	tr.Report(&b)
	if !strings.Contains(b.String(), "not recorded") {
		t.Errorf("the operator must be told nothing was recorded, got %q", b.String())
	}
}

// The path is reported on success too. A run that worked is also the one an operator comes
// back to when a problem surfaces later.
func TestTranscript_ReportNamesThePath(t *testing.T) {
	tr := newTranscript(t.TempDir(), "apply")
	defer tr.Close()

	var b strings.Builder
	tr.Report(&b)
	if !strings.Contains(b.String(), tr.Path()) {
		t.Errorf("the report must name the file, got %q", b.String())
	}
}

// Transcripts live beside the checkouts rackctl already manages for the same org, so an
// operator looking for one run's output finds it next to that org's state.
func TestTranscriptDir_IsScopedToTheOrg(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	got := transcriptDir("acme")
	if want := filepath.Join(home, ".rackctl", "acme", "logs"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// An org-less run still gets a transcript rather than none.
	if transcriptDir("") == "" {
		t.Error("an empty org must still resolve a directory")
	}
}
