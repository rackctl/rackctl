package exec

import (
	"bytes"
	"context"
	"strings"
	"testing"
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
