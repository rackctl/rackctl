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

// Query runs a READ-ONLY command and returns trimmed stdout, executing even in dry-run.
//
// Capture returns "" in dry-run, which is correct for a query whose answer decides what to
// CHANGE — a dry-run must not branch on live state it is only pretending to read. It is
// wrong for the enumeration half of a destructive sweep, and wrong in a way that hides the
// only question a dry-run of a sweep is asked: what would this select?
//
// Every sweep in internal/reap used to answer that with a sentence. `rackctl destroy`
// without --apply printed "force-delete operator-minted IAM roles under
// /eks-agent-platform/tenants/ named <cluster>-*" and queried nothing, so it could describe
// its own filter and never demonstrate it. In an account holding one estate that is a
// cosmetic gap. In an account holding three it is the difference between a checkable claim
// and a promise — the sweep cannot be shown to select zero pre-existing resources, because
// in dry-run it selects nothing at all, for the wrong reason.
//
// So: enumeration is read-only and always executes; mutation goes through Run and respects
// DryRun. That split is what makes `destroy` (no --apply) a real negative test.
//
// Callers must only pass list/describe/get verbs. Nothing here enforces that, and nothing
// can — it is a contract, and the reason this is a separate method rather than a flag on
// Capture is so the contract is visible at every call site.
// Query deliberately does NOT inherit Runner.Dir, and that is not a shortcut.
//
// Runner.Dir is a repo checkout — `rackctl destroy` sets it to
// ~/.rackctl/<org>/landing-zone so terragrunt runs in the right tree. A cloud API query's
// answer cannot depend on the caller's working directory, but exec.Cmd's does: an absent Dir
// makes the process fail to start at all. So a missing checkout made every enumeration in
// internal/reap return a chdir error, which each sweep treated as "no credentials, nothing to
// do" and swallowed. The sweep then printed nothing and moved on, and a dry-run pointed at a
// live account proved exactly nothing while looking clean.
//
// That is the failure this whole change exists to remove — a gate that is green because it
// never ran. Not inheriting Dir means the enumeration works wherever it is called from, which
// for a cloud API is the only correct behaviour.
func (r *Runner) Query(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
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
