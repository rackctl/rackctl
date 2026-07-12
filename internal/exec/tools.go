// Package exec wraps the external tools rackctl orchestrates (tofu, terragrunt,
// kubectl, helm, aws, git). In dry-run mode commands are printed, not executed.
package exec

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Runner shells out to external tools.
type Runner struct {
	DryRun bool
	Dir    string   // working directory for commands
	Env    []string // extra environment (appended to os.Environ)
	Out    io.Writer
}

// New returns a Runner writing to out.
func New(out io.Writer) *Runner {
	if out == nil {
		out = os.Stdout
	}
	return &Runner{Out: out}
}

// Run executes name+args, streaming output. In dry-run it prints the command.
func (r *Runner) Run(ctx context.Context, name string, args ...string) error {
	line := name + " " + strings.Join(args, " ")
	if r.DryRun {
		fmt.Fprintf(r.Out, "    → (dry-run) %s\n", line)
		return nil
	}
	fmt.Fprintf(r.Out, "    → %s\n", line)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = r.Dir
	if len(r.Env) > 0 {
		cmd.Env = append(os.Environ(), r.Env...)
	}
	cmd.Stdout = r.Out
	cmd.Stderr = r.Out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// Capture runs name+args and returns trimmed stdout. Returns "" in dry-run.
func (r *Runner) Capture(ctx context.Context, name string, args ...string) (string, error) {
	if r.DryRun {
		return "", nil
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = r.Dir
	if len(r.Env) > 0 {
		cmd.Env = append(os.Environ(), r.Env...)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return strings.TrimSpace(out.String()), nil
}

// RequireTools verifies the given executables are on PATH.
func RequireTools(names ...string) error {
	var missing []string
	for _, n := range names {
		if _, err := exec.LookPath(n); err != nil {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required tools: %s", strings.Join(missing, ", "))
	}
	return nil
}
