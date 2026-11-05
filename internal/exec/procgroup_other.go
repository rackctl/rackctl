//go:build !unix

package exec

import (
	"io"
	"os/exec"
	"time"
)

// interruptible keeps CommandContext's default on a platform with no process groups.
func interruptible(*exec.Cmd, io.Writer, string, time.Duration) (stop func()) { return func() {} }

// killGroup has no group to end on a platform without them.
func killGroup(int) {}
