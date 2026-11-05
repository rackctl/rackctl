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

// fakeAWS puts an aws on PATH that records its argv and runs body.
func fakeAWS(t *testing.T, body string) func() string {
	t.Helper()
	dir := t.TempDir()
	logf := filepath.Join(dir, "calls")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + logf + "\n" + body
	if err := os.WriteFile(filepath.Join(dir, "aws"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() string { b, _ := os.ReadFile(logf); return string(b) }
}

// A dry-run repoints nothing the operator owns: it writes a temporary kubeconfig aimed at the
// cluster by name, points this Runner's subprocesses at it, and removes it on release. The
// read happens for real, so the in-cluster preview reads the right cluster rather than
// whatever the operator's context points at.
func TestPointAt_ADryRunWritesATemporaryKubeconfigAndRemovesIt(t *testing.T) {
	calls := fakeAWS(t, "exit 0\n")
	run := exec.New(&bytes.Buffer{})
	run.DryRun = true
	run.Env = []string{"TF_VAR_x=1"}

	release, err := PointAt(context.Background(), run, "staging-platform", "us-west-2")
	if err != nil {
		t.Fatal(err)
	}
	var path string
	for _, e := range run.Env {
		if v, ok := strings.CutPrefix(e, "KUBECONFIG="); ok {
			path = v
		}
	}
	if path == "" {
		t.Fatalf("the Runner's subprocesses must read the temporary kubeconfig: %v", run.Env)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the temporary kubeconfig must exist while it is in use: %v", err)
	}
	if got := calls(); !strings.Contains(got, "eks update-kubeconfig --name staging-platform --kubeconfig "+path+" --region us-west-2") {
		t.Errorf("the kubeconfig must be written to the temporary file, never the operator's: %q", got)
	}
	release()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the temporary kubeconfig survived its release: %v", err)
	}
	if len(run.Env) != 1 || run.Env[0] != "TF_VAR_x=1" {
		t.Errorf("release must restore the Runner's env: %v", run.Env)
	}
}

// Every way a dry-run repoint can fail leaves nothing behind and reports the failure.
func TestPointAt_ADryRunThatCannotRepointLeavesNothing(t *testing.T) {
	run := exec.New(&bytes.Buffer{})
	run.DryRun, run.ReadAttempts = true, 1
	if _, err := PointAt(context.Background(), run, "", "r"); err == nil {
		t.Error("a blank cluster must fail")
	}

	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	fakeAWS(t, "echo 'An error occurred (ResourceNotFoundException)' >&2\nexit 254\n")
	if _, err := PointAt(context.Background(), run, "c", ""); err == nil || !strings.Contains(err.Error(), "ResourceNotFoundException") {
		t.Errorf("a repoint that failed must say why: %v", err)
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("a failed repoint left a temporary kubeconfig: %v", left)
	}

	t.Setenv("TMPDIR", filepath.Join(tmp, "missing"))
	if _, err := PointAt(context.Background(), run, "c", "r"); err == nil || !strings.Contains(err.Error(), "temporary kubeconfig") {
		t.Errorf("a temporary file that cannot be written must say so: %v", err)
	}
}

// A real run repoints the operator's kubeconfig, which is what the sweeps act through.
func TestPointAt_ARealRunRepointsTheKubeconfig(t *testing.T) {
	calls := fakeAWS(t, "exit 0\n")
	release, err := PointAt(context.Background(), exec.New(&bytes.Buffer{}), "staging-platform", "us-west-2")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if got := calls(); !strings.Contains(got, "eks update-kubeconfig --name staging-platform") || strings.Contains(got, "--kubeconfig") {
		t.Errorf("got %q", got)
	}
}
