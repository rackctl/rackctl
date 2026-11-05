//go:build unix

package exec

import (
	"fmt"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// interruptible puts a mutating child in its own process group and replaces
// CommandContext's SIGKILL with ONE SIGINT to that group, followed by SIGKILL to the group
// only if it is still running grace later. It returns a func that cancels that fallback
// once the child has exited.
//
// A SIGKILL mid-apply is the worst way to stop tofu. It holds the state lock and has
// resources in flight, and a killed process releases neither: the rollback that follows
// then fails on the lock this run still holds. One SIGINT is tofu's graceful stop, which
// finishes the in-flight operations, writes state and releases the lock.
//
// The process group is what makes it exactly one. In the child's original group a Ctrl-C
// reaches it from the terminal AND from here, and tofu treats a second interrupt as a forced
// stop, which is the SIGKILL outcome again. Its own group takes it out of the terminal's
// reach, so the only interrupt it sees is this one, and the group, not the pid, is signalled
// because terragrunt runs tofu as a child of its own.
func interruptible(cmd *exec.Cmd, out io.Writer, name string, grace time.Duration) (stop func()) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var mu sync.Mutex
	var fallback *time.Timer
	cmd.Cancel = func() error {
		pgid := cmd.Process.Pid
		fmt.Fprintf(out, "    → interrupted: waiting up to %s for %s to release state\n", grace, name)
		mu.Lock()
		fallback = time.AfterFunc(grace, func() { killGroup(pgid) })
		mu.Unlock()
		return syscall.Kill(-pgid, syscall.SIGINT)
	}
	// Bounds the wait for the child's output pipes once the group is gone: a grandchild that
	// escaped the group could otherwise hold them open and Wait would never return.
	cmd.WaitDelay = grace + 10*time.Second
	return func() {
		mu.Lock()
		defer mu.Unlock()
		if fallback != nil {
			fallback.Stop()
		}
	}
}

// killGroup ends a process group outright. It is the second interrupt, forwarded.
func killGroup(pgid int) { _ = syscall.Kill(-pgid, syscall.SIGKILL) }
