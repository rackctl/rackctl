package tui

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// transcript is where the subprocess output goes while the TUI owns the terminal.
//
// The TUI renders one line per phase, so the terragrunt apply logs, kubectl errors and
// helm output cannot go to stdout — they would fight the view for the screen. Discarding
// them is the other obvious choice and it is worse: a forty-minute install that fails
// leaves the operator with a cross beside a phase name and nothing else. The output IS
// the diagnosis.
//
// So it is written to a file and the path is printed when the view exits, on success and
// on failure alike. It also keeps the most recent line in memory, which the view renders
// under the running phase — a long wait then shows what it is waiting on rather than a
// spinner alone.
type transcript struct {
	mu   sync.Mutex
	f    *os.File
	last string
	path string
}

// newTranscript opens a transcript beside the operator's other rackctl state.
//
// A failure to open one is NOT fatal, and that asymmetry is deliberate: the transcript is
// a diagnostic aid, and refusing to provision because a log file could not be created
// would make the aid more important than the thing it documents. The run proceeds with an
// in-memory tail only, and says so.
func newTranscript(dir, name string) *transcript {
	t := &transcript{}
	if dir == "" {
		return t
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return t
	}
	f, err := os.CreateTemp(dir, name+"-*.log")
	if err != nil {
		return t
	}
	t.f, t.path = f, f.Name()
	return t
}

// Write records the bytes and keeps the last non-empty line for the view.
func (t *transcript) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.f != nil {
		_, _ = t.f.Write(p)
	}
	for _, line := range strings.Split(string(p), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			t.last = s
		}
	}
	return len(p), nil
}

// Tail is the most recent line of subprocess output, truncated to width RUNES.
//
// Runes rather than bytes: the caller's width is a column count, and a tail wider than the
// terminal wraps and shifts the whole view. Slicing bytes would also cut a multi-byte
// character in half, and the ellipsis is itself three bytes — so a byte-based limit
// overshoots the width it was given by exactly the marker meant to keep it inside.
func (t *transcript) Tail(width int) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := []rune(t.last)
	if width > 0 && len(r) > width {
		return string(r[:width-1]) + "…"
	}
	return t.last
}

// Path is where the transcript was written, or "" if none could be opened.
func (t *transcript) Path() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.path
}

func (t *transcript) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.f != nil {
		_ = t.f.Close()
	}
}

// Report names the transcript, or explains its absence. Printed after the view exits, so
// it survives the alternate screen buffer the TUI runs in.
func (t *transcript) Report(w io.Writer) {
	if p := t.Path(); p != "" {
		fmt.Fprintf(w, "transcript: %s\n", p)
		return
	}
	fmt.Fprintln(w, "transcript: none — no log directory could be opened, so this run's "+
		"subprocess output was not recorded")
}

// transcriptDir is where a run's transcripts live, beside the checkouts rackctl manages
// for the same org. Empty when the home directory cannot be resolved, which the caller
// treats as "no transcript" rather than as a failure.
func transcriptDir(org string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	if org == "" {
		org = "rackctl"
	}
	return filepath.Join(home, ".rackctl", org, "logs")
}
