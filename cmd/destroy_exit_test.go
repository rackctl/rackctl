package cmd

import (
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

// The second interrupt is forwarded. A mutating child runs in its own process group, so the
// terminal's Ctrl-C does not reach it, and a rackctl that simply died would leave tofu
// running with nothing to report to. The stub ignores the first interrupt, as a tofu still
// writing state does, so only the forwarded kill can end it.
func TestDestroy_ASecondInterruptKillsTheToolAndExitsResourcesMayRemain(t *testing.T) {
	sleep, err := osexec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep on PATH")
	}
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "pid")
	cmd := destroyCommand(t, map[string]string{
		"terragrunt": "trap '' INT\necho $$ > " + pidfile + "\nexec " + sleep + " 60\n",
	}, "--yes")
	out, err := os.CreateTemp(dir, "out")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = out.Close() }()
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var pid int
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if b, err := os.ReadFile(pidfile); err == nil && strings.TrimSpace(string(b)) != "" {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatal("terragrunt never started")
		}
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	code := exitStatus(cmd.Wait())
	b, _ := os.ReadFile(out.Name())
	if code != ExitStranded {
		t.Fatalf("want exit %d, got %d\n%s", ExitStranded, code, b)
	}
	if !strings.Contains(string(b), "forced exit on a second interrupt") {
		t.Errorf("the message must say why it stopped:\n%s", b)
	}
	for deadline := time.Now().Add(5 * time.Second); processAlive(pid); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("the tool outlived rackctl — the second interrupt was not forwarded to its group")
		}
	}
}

// processAlive reports whether pid is running, counting a zombie as gone.
func processAlive(pid int) bool {
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		return false
	}
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	f := strings.Fields(string(stat))
	return len(f) < 3 || f[2] != "Z"
}

// A dry-run shows the refusal the real run would hit, because it reads the hub for real
// through a temporary kubeconfig. It used to read nothing, so a rehearsal never refused.
func TestDestroy_ADryRunShowsTheSpokeRefusal(t *testing.T) {
	out, err := destroyCommand(t, map[string]string{
		"kubectl": "case \"$*\" in *clusters.fleet*) echo 'fleet/spoke-a' ;; esac\nexit 0\n",
	}, "--dry-run").CombinedOutput()
	if code := exitStatus(err); code != ExitRefused {
		t.Fatalf("want exit %d, got %d\n%s", ExitRefused, code, out)
	}
	if !strings.Contains(string(out), "fleet/spoke-a") || !strings.Contains(string(out), "your kubeconfig is untouched") {
		t.Errorf("the dry-run refusal must name the spoke and say nothing changed:\n%s", out)
	}
}

// A dry-run writes a temporary kubeconfig, reads the cluster through it, and removes it on
// exit; the operator's own kubeconfig is never written.
func TestDestroy_ADryRunReadsThroughATemporaryKubeconfig(t *testing.T) {
	dir := t.TempDir()
	seen := filepath.Join(dir, "seen")
	cmd := destroyCommand(t, map[string]string{
		"aws":     "case \"$*\" in *update-kubeconfig*--kubeconfig*) echo \"$*\" >> " + seen + " ;; *update-kubeconfig*) echo REPOINTED >> " + seen + " ;; esac\nexit 0\n",
		"kubectl": "echo \"KUBECONFIG=$KUBECONFIG\" >> " + seen + "\nexit 0\n",
	}, "--dry-run")
	out, err := cmd.CombinedOutput()
	if code := exitStatus(err); code != 0 {
		t.Fatalf("want exit 0, got %d\n%s", code, out)
	}
	b, _ := os.ReadFile(seen)
	got := string(b)
	if strings.Contains(got, "REPOINTED") {
		t.Fatalf("a dry-run repointed the operator's kubeconfig:\n%s", got)
	}
	var tmp string
	for _, l := range strings.Split(got, "\n") {
		if v, ok := strings.CutPrefix(l, "KUBECONFIG="); ok {
			if v == "" {
				t.Fatalf("kubectl read the ambient kubeconfig in a dry-run:\n%s", got)
			}
			tmp = v
		}
	}
	if tmp == "" || !strings.Contains(got, "--kubeconfig "+tmp) {
		t.Fatalf("kubectl must read the temporary kubeconfig the dry-run wrote:\n%s", got)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("the temporary kubeconfig %s survived the dry-run", tmp)
	}
	if !strings.Contains(string(out), "would delete 0 persistentvolumeclaims") {
		t.Errorf("the dry-run must preview the in-cluster reap:\n%s", out)
	}
}

// A second interrupt before anything mutating started touched nothing, so it is aborted,
// not resources-may-remain. At the confirmation prompt the first Ctrl-C only cancels the
// context, which the prompt does not read, so the operator presses it again; that sent a
// script to a human, and told the operator to destroy, over a command that changed nothing.
func TestDestroy_ASecondInterruptAtThePromptIsAborted(t *testing.T) {
	cmd := destroyCommand(t, map[string]string{})
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close() }()
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
		if b, _ := os.ReadFile(out.Name()); strings.Contains(string(b), "Type the cluster name to confirm") {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatal("the prompt never appeared")
		}
	}
	for i := 0; i < 2; i++ {
		if err := cmd.Process.Signal(os.Interrupt); err != nil {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	code := exitStatus(cmd.Wait())
	b, _ := os.ReadFile(out.Name())
	if code != ExitAborted {
		t.Fatalf("want exit %d, got %d\n%s", ExitAborted, code, b)
	}
	if !strings.Contains(string(b), "nothing was touched") || strings.Contains(string(b), "rackctl destroy` to remove") {
		t.Errorf("the message must say nothing was touched, and send nobody to destroy:\n%s", b)
	}
}
