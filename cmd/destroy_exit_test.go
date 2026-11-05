package cmd

import (
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A component that failed to destroy leaves everything behind it standing, which is the
// state resources-may-remain names. It exited 1 — the same status as a config that did not
// load — so an automation could not tell a partial teardown from a run that touched nothing.
func TestDestroy_APartialTeardownExitsResourcesMayRemain(t *testing.T) {
	out, err := destroyCommand(t, map[string]string{
		"terragrunt": "echo 'Error: DependencyViolation' >&2\nexit 1\n",
	}, "--yes").CombinedOutput()
	if code := exitStatus(err); code != ExitStranded {
		t.Fatalf("want exit %d, got %d\n%s", ExitStranded, code, out)
	}
	if !strings.Contains(string(out), "resources-may-remain (exit 7)") || strings.Contains(string(out), "platform destroyed") {
		t.Errorf("the status must be named, and the teardown must not claim success:\n%s", out)
	}
}

// The eks-fleet guard refuses before anything is destroyed. That is a refusal with nothing
// touched, not a teardown that stopped partway, and it says the one thing it did change.
func TestDestroy_TheSpokeRefusalExitsRefused(t *testing.T) {
	mark := filepath.Join(t.TempDir(), "terragrunt-ran")
	out, err := destroyCommand(t, map[string]string{
		"kubectl":    "case \"$*\" in *clusters.fleet*) echo 'fleet/spoke-a' ;; esac\nexit 0\n",
		"terragrunt": ": > " + mark + "\nexit 0\n",
	}, "--yes").CombinedOutput()
	if code := exitStatus(err); code != ExitRefused {
		t.Fatalf("want exit %d, got %d\n%s", ExitRefused, code, out)
	}
	for _, want := range []string{"refused (exit 9)", "fleet/spoke-a", "Nothing was destroyed",
		"kubectl's current context now points at development-platform"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(mark); err == nil {
		t.Error("terragrunt ran after the refusal")
	}
}

// interruptDestroy starts a destroy whose stub for tool records that it started and then
// blocks, interrupts the process once it has, and returns what it printed and how it exited.
func interruptDestroy(t *testing.T, tool string) (string, int) {
	t.Helper()
	sleep, err := osexec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep on PATH")
	}
	mark := filepath.Join(t.TempDir(), "started")
	cmd := destroyCommand(t, map[string]string{tool: ": > " + mark + "\nexec " + sleep + " 30\n"}, "--yes")
	out, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = out.Close() }()
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(mark); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("%s never started", tool)
		}
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	code := exitStatus(cmd.Wait())
	b, err := os.ReadFile(out.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(b), code
}

// An interrupt before anything destructive began is an abort with nothing to unwind. One
// after the teardown began leaves components standing, and exits resources-may-remain, with
// the message saying why it stopped.
func TestDestroy_AnInterruptIsAbortedOnlyBeforeAnythingWasDestroyed(t *testing.T) {
	t.Run("during the repoint", func(t *testing.T) {
		out, code := interruptDestroy(t, "aws")
		if code != ExitAborted {
			t.Fatalf("want exit %d, got %d\n%s", ExitAborted, code, out)
		}
		if !strings.Contains(out, "interrupted before anything was destroyed") {
			t.Errorf("the message must say nothing was destroyed:\n%s", out)
		}
	})
	t.Run("during the component teardown", func(t *testing.T) {
		out, code := interruptDestroy(t, "terragrunt")
		if code != ExitStranded {
			t.Fatalf("want exit %d, got %d\n%s", ExitStranded, code, out)
		}
		if !strings.Contains(out, "resources-may-remain (exit 7): interrupted: ") {
			t.Errorf("the message must say the teardown was interrupted:\n%s", out)
		}
	})
}
