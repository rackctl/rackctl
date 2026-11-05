//go:build unix

package exec

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeTool writes an executable named name onto PATH with the given body.
func fakeTool(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

// waitFor polls until path exists.
func waitFor(t *testing.T, path string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared", path)
		}
	}
}

// alive reports whether pid is a running process, counting a zombie as gone.
func alive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	fields := strings.Fields(string(stat))
	return len(fields) < 3 || fields[2] != "Z"
}

// diesWithin reports whether pid is gone, or a zombie, before d runs out.
func diesWithin(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for alive(pid) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}

// A cancelled mutation gets ONE SIGINT and time to release its state, not SIGKILL.
//
// tofu treats a first interrupt as a graceful stop and a second as a forced one, and a
// killed tofu holds the state lock the rollback then needs. The fake records every SIGINT
// it receives and keeps running past it, so a second signal would show as a second line,
// and a SIGKILL as none at all.
func TestRun_ACancelIsOneSIGINTAndAGraceToStop(t *testing.T) {
	dir := t.TempDir()
	got, started := filepath.Join(dir, "ints"), filepath.Join(dir, "started")
	fakeTool(t, "tofu", `trap 'echo int >> `+got+`' INT
: > `+started+`
i=0
while [ $i -lt 20 ]; do sleep 0.05; i=$((i+1)); done
exit 3
`)
	var out bytes.Buffer
	r := New(&syncWriter{w: &out})
	r.InterruptGrace = 30 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	go func() { waitFor(t, started); cancel() }()
	if err := r.Run(ctx, "tofu", "apply"); err == nil {
		t.Fatal("a cancelled run must fail")
	}
	b, _ := os.ReadFile(got)
	if n := strings.Count(string(b), "int"); n != 1 {
		t.Fatalf("the child saw %d SIGINT(s), want exactly 1 — none is a SIGKILL, which leaves the "+
			"state lock held, and two is tofu's forced stop", n)
	}
	if !strings.Contains(out.String(), "waiting up to 30s for tofu to release state") {
		t.Errorf("the wait must be said, with its bound and the tool:\n%s", out.String())
	}
}

// A child that ignores the SIGINT is killed when the grace runs out, and its whole process
// group with it: terragrunt runs tofu as a child of its own, and killing only the leader
// leaves tofu running with nothing to report to.
func TestRun_AChildThatIgnoresTheInterruptLosesItsGroupAtTheGrace(t *testing.T) {
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "grandchild")
	fakeTool(t, "terragrunt", `trap '' INT
sleep 30 &
echo $! > `+pidfile+`
wait
`)
	r := New(&syncWriter{w: &bytes.Buffer{}})
	r.InterruptGrace = 200 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	go func() { waitFor(t, pidfile); cancel() }()
	start := time.Now()
	if err := r.Run(ctx, "terragrunt", "apply"); err == nil {
		t.Fatal("a cancelled run must fail")
	}
	if took := time.Since(start); took > 15*time.Second {
		t.Fatalf("Run took %s against a 200ms grace", took)
	}
	b, _ := os.ReadFile(pidfile)
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	// The group's SIGKILL is queued for every member at once, but Run returns as soon as the
	// leader is reaped, and a loaded machine may not have scheduled the grandchild's death
	// yet. A grandchild outside the group never dies, so a bounded wait still catches that.
	if !diesWithin(pid, 5*time.Second) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatal("the grandchild survived the grace — only the leader was killed, not its group")
	}
}

// The second interrupt: every mutating child is killed, group and all, and nothing new is
// started on the way out.
func TestForceStop_KillsEveryChildAndRefusesTheNext(t *testing.T) {
	t.Cleanup(func() {
		children.Lock()
		children.stopped = false
		children.Unlock()
	})
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	fakeTool(t, "tofu", "trap '' INT\n: > "+started+"\nwhile :; do sleep 0.05; done\n")
	r := New(&syncWriter{w: &bytes.Buffer{}})

	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background(), "tofu", "apply") }()
	waitFor(t, started)
	ForceStop()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a force-stopped child must fail its call")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ForceStop did not end the running child")
	}
	if err := r.Run(context.Background(), "tofu", "destroy"); !errors.Is(err, ErrForceStopped) {
		t.Fatalf("a call after ForceStop must not start: %v", err)
	}
}

// Only a mutating child counts as a mutation: a read, a dry-run and a refused start do
// not, so a forced exit before one can say that nothing was touched.
func TestMutationStarted_CountsOnlyAMutatingChild(t *testing.T) {
	children.Lock()
	prev := children.mutated
	children.mutated = false
	children.Unlock()
	t.Cleanup(func() {
		children.Lock()
		children.mutated = prev
		children.Unlock()
	})
	fakeTool(t, "tofu", "exit 0\n")
	ctx := context.Background()
	r := New(&syncWriter{w: &bytes.Buffer{}})
	if _, err := r.Capture(ctx, "tofu", "state", "list"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Query(ctx, "tofu", "output"); err != nil {
		t.Fatal(err)
	}
	r.DryRun = true
	if err := r.Run(ctx, "tofu", "apply"); err != nil {
		t.Fatal(err)
	}
	if MutationStarted() {
		t.Fatal("a read or a dry-run was counted as a mutation")
	}
	r.DryRun = false
	if err := r.Run(ctx, "tofu", "apply"); err != nil {
		t.Fatal(err)
	}
	if !MutationStarted() {
		t.Fatal("a mutating child was not counted")
	}
}
