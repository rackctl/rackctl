package exec

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Query must work from a Runner whose Dir does not exist.
//
// This is a regression test for a silent no-op, which is the failure class this repo keeps
// finding: `rackctl destroy` sets Runner.Dir to ~/.rackctl/<org>/landing-zone so terragrunt
// runs in the right tree, and every reap sweep then enumerated through that same Runner. With
// no checkout on disk, exec.Cmd could not start the process at all, so `aws iam list-roles`
// came back as a chdir error — which each sweep treated as "no credentials, nothing to do"
// and swallowed. A dry-run pointed at a live account printed a clean teardown having queried
// nothing.
//
// A cloud API's answer cannot depend on the caller's working directory. Anything that makes
// it appear to is a bug, and this pins it.
func TestQuery_RunsWithNoWorkingDirectory(t *testing.T) {
	r := New(&bytes.Buffer{})
	r.Dir = "/definitely/not/a/real/checkout/anywhere"

	out, err := r.Query(context.Background(), "echo", "reached")
	if err != nil {
		t.Fatalf("Query must not inherit Dir — a missing checkout turned every read-only cloud "+
			"enumeration into a swallowed error, which reads as an empty selection: %v", err)
	}
	if strings.TrimSpace(out) != "reached" {
		t.Fatalf("got %q, want %q", out, "reached")
	}
}

// Capture keeps inheriting Dir, because its callers are repo operations (git, terragrunt)
// where the directory IS the meaning. The two must not be collapsed.
func TestCapture_StillInheritsDir(t *testing.T) {
	r := New(&bytes.Buffer{})
	r.Dir = "/definitely/not/a/real/checkout/anywhere"

	if _, err := r.Capture(context.Background(), "echo", "reached"); err == nil {
		t.Fatal("Capture must still run in Dir — its callers are git/terragrunt operations where " +
			"the working directory is the whole point")
	}
}

// The split that makes a dry-run a negative test: mutation is suppressed, enumeration is not.
func TestDryRun_SuppressesRunButNotQuery(t *testing.T) {
	var out bytes.Buffer
	r := New(&out)
	r.DryRun = true

	if err := r.Run(context.Background(), "false"); err != nil {
		t.Fatalf("dry-run Run must not execute: %v", err)
	}
	if !strings.Contains(out.String(), "(dry-run)") {
		t.Fatalf("dry-run Run must print what it would have done.\n%s", out.String())
	}

	got, err := r.Query(context.Background(), "echo", "executed")
	if err != nil || strings.TrimSpace(got) != "executed" {
		t.Fatalf("dry-run Query MUST execute — a dry-run that cannot enumerate cannot show that "+
			"a destructive sweep selects nothing, which is the only thing it is asked. got %q, err %v",
			got, err)
	}

	if _, err := r.Capture(context.Background(), "echo", "suppressed"); err != nil {
		t.Fatalf("dry-run Capture stays suppressed: %v", err)
	}
}

// A subprocess that never exits must end at the deadline rather than hold the pipeline.
//
// This is the invariant behind every ceiling in this package: a phase waiting on a wedged
// tool cannot fail, cannot roll back, and cannot report — the operator sees a cursor. The
// error has to name the deadline too, because a caller that renders a timeout as a plain
// non-zero exit sends the operator looking for output the tool never produced.
func TestQuery_WedgedToolEndsAtTheDeadline(t *testing.T) {
	r := New(&bytes.Buffer{})
	r.QueryTimeout = 100 * time.Millisecond

	start := time.Now()
	_, err := r.Query(context.Background(), "sleep", "60")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a command that outlives its ceiling must fail, not return successfully")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("Query ran for %s against a 100ms ceiling — the deadline is not being applied", elapsed)
	}
	if !strings.Contains(err.Error(), "giving up") {
		t.Errorf("the error must say the deadline ended it, not just that the tool exited: %v", err)
	}
}

// Run carries its own, longer ceiling: a terragrunt apply is legitimately slow, and giving
// mutation the read ceiling would abort real work. Both are bounded; only the numbers differ.
func TestRun_CarriesItsOwnCeiling(t *testing.T) {
	r := New(&bytes.Buffer{})
	r.RunTimeout = 100 * time.Millisecond

	start := time.Now()
	err := r.Run(context.Background(), "sleep", "60")
	if err == nil {
		t.Fatal("Run must be bounded too — an unbounded apply is the case the ceilings exist for")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Run ran for %s against a 100ms ceiling", elapsed)
	}
}

// An interrupt and an expired ceiling are different failures with different remedies, so
// they must not render as the same sentence. Ctrl-C is the operator's decision and needs no
// diagnosis; a ceiling means the tool stopped answering and the operator has something to
// investigate.
func TestQuery_InterruptIsNotReportedAsATimeout(t *testing.T) {
	r := New(&bytes.Buffer{})
	r.QueryTimeout = time.Minute

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()

	_, err := r.Query(ctx, "sleep", "60")
	if err == nil {
		t.Fatal("a cancelled context must fail the call")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("an interrupt must surface as context.Canceled so callers can tell it from a "+
			"wedged tool: %v", err)
	}
	if strings.Contains(err.Error(), "giving up") {
		t.Errorf("an interrupt was reported as a ceiling expiry: %v", err)
	}
}

// stderr is the only place aws, kubectl and git say WHY a call failed. Discarding it leaves
// every caller wrapping "exit status 1", and thirteen operator-facing messages in this repo
// interpolate that wrapped error into a sentence promising an explanation.
func TestCapture_ErrorCarriesTheToolsOwnDiagnostic(t *testing.T) {
	r := New(&bytes.Buffer{})

	_, err := r.Capture(context.Background(), "sh", "-c", "echo AccessDeniedException >&2; exit 254")
	if err == nil {
		t.Fatal("a non-zero exit must fail the call")
	}
	if !strings.Contains(err.Error(), "AccessDeniedException") {
		t.Errorf("the tool's own stderr must reach the caller, or the error says only that "+
			"something exited: %v", err)
	}
}

// stderr must not reach stdout. Every Capture caller parses the returned string — as JSON,
// as a jsonpath result, as a bare ARN — so merging the streams would corrupt the value on
// the path where nothing went wrong.
func TestCapture_StderrDoesNotContaminateStdout(t *testing.T) {
	r := New(&bytes.Buffer{})

	out, err := r.Capture(context.Background(), "sh", "-c", "echo noise >&2; echo value")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "value" {
		t.Fatalf("got %q, want %q — stderr leaked into the parsed value", out, "value")
	}
}
