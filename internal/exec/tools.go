// Package exec wraps the external tools rackctl orchestrates (tofu, terragrunt,
// kubectl, helm, aws, git). In dry-run mode commands are printed, not executed.
package exec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Timeout ceilings. Every subprocess runs under one: a tool that has stopped making
// progress must fail the phase that is waiting on it rather than hold the pipeline open
// indefinitely, because a hung child is indistinguishable from a slow one to the operator
// and neither the phase nor the rollback can proceed past it.
//
// The two ceilings differ because the two call classes differ by orders of magnitude.
const (
	// DefaultRunTimeout bounds one mutating invocation. The longest is a terragrunt
	// apply of the cluster component, which waits on an EKS control plane; AWS documents
	// that as up to 15 minutes and the leaf applies more besides. An hour is above any
	// single component's honest worst case and far below "forever".
	DefaultRunTimeout = time.Hour

	// DefaultQueryTimeout bounds one read. Every Capture and Query is a list, describe or
	// get against a cloud API, a local git tree or a kubernetes API server. A read that
	// has not answered in two minutes is wedged, not slow — kubectl calls in this repo
	// already carry --request-timeout=30s for the same reason.
	DefaultQueryTimeout = 2 * time.Minute

	// DefaultReadAttempts is how many times a READ is tried before its error stands.
	//
	// Reads only, and the asymmetry is the whole design. A cloud API answering
	// ThrottlingException or a resolver dropping one packet is the case retry exists for,
	// and a list or describe is idempotent by construction — asking twice costs a request.
	// A MUTATION is not: re-running a terragrunt apply that failed halfway is a second
	// apply against state the first one already moved, and an installer that does that on
	// the operator's behalf is worse than one that stops and says so. Run therefore never
	// retries, and that is deliberate rather than unfinished.
	//
	// Three, because the failures worth retrying are transient in seconds — a throttle, a
	// re-elected endpoint, a DNS blip. A read still failing on the third attempt is not
	// slow, it is wrong, and more attempts only delay saying so.
	DefaultReadAttempts = 3

	// retryBase is the first backoff. It doubles per attempt and carries jitter, so a
	// phase issuing many reads against one throttled API does not re-issue them in lockstep
	// and reproduce the throttle it is backing off from.
	retryBase = 500 * time.Millisecond
)

// retryable reports whether a failed read is worth trying again.
//
// Deliberately narrow. A retry loop that cannot tell a throttle from a permission error
// turns a clear AccessDenied into the same error three attempts later, and turns a missing
// resource into a slow missing resource — so this matches the transient shapes by name and
// treats everything else as final. A timeout of our own is also not retryable: the ceiling
// already expressed how long the caller was willing to wait.
func retryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	s := err.Error()
	for _, transient := range []string{
		"ThrottlingException", "Throttling", "TooManyRequestsException",
		"RequestLimitExceeded", "SlowDown", "ServiceUnavailable",
		"InternalError", "InternalFailure", "RequestTimeout",
		"connection reset", "connection refused", "no such host",
		"i/o timeout", "TLS handshake timeout", "unexpected EOF",
		"etcdserver: request timed out", "the server is currently unable to handle the request",
	} {
		if strings.Contains(s, transient) {
			return true
		}
	}
	return false
}

// backoff is the delay before attempt n (1-based), doubling with jitter.
//
// The jitter is not cosmetic: a phase that issues a dozen reads against one throttled API
// and backs them all off by the same interval re-issues them together and reproduces the
// throttle. Full jitter over the doubled window is the standard fix.
func backoff(n int) time.Duration {
	window := retryBase << (n - 1)
	return time.Duration(rand.Int64N(int64(window)) + int64(retryBase))
}

// Runner shells out to external tools.
type Runner struct {
	DryRun bool
	Dir    string   // working directory for commands
	Env    []string // extra environment (appended to os.Environ)
	Out    io.Writer

	// EnvSource resolves the identity a subprocess runs as, once per invocation.
	//
	// It exists because a long apply outlives its credentials. An assumed STS session is
	// 1h by default and an EKS control plane alone takes a quarter of that; resolving the
	// identity once and freezing it into Env means the run dies mid-phase on an expired
	// token, with the rollback needing the same credentials it just lost. Consulted per
	// call, the identity provider can re-assume before expiry — which is what makes its
	// refresh window reachable rather than decorative.
	//
	// Its entries are placed BEFORE Env, so a variable scoped to one invocation still
	// wins. An error fails the call: running as the wrong principal is worse than not
	// running.
	EnvSource func(context.Context) ([]string, error)

	// RunTimeout overrides DefaultRunTimeout for Run. Zero takes the default.
	RunTimeout time.Duration
	// QueryTimeout overrides DefaultQueryTimeout for Capture and Query. Zero takes
	// the default.
	QueryTimeout time.Duration
	// ReadAttempts overrides DefaultReadAttempts. Zero takes the default; 1 disables
	// retry, which is what a test asserting a single invocation wants.
	ReadAttempts int
}

// New returns a Runner writing to out.
func New(out io.Writer) *Runner {
	if out == nil {
		out = os.Stdout
	}
	return &Runner{Out: out}
}

// env composes the environment for one invocation: the process environment, then the
// identity EnvSource resolves now, then Env — later entries win, so a scoped TF_VAR beats
// an ambient one.
func (r *Runner) env(ctx context.Context) ([]string, error) {
	if r.EnvSource == nil {
		if len(r.Env) == 0 {
			return nil, nil
		}
		return append(os.Environ(), r.Env...), nil
	}
	identity, err := r.EnvSource(ctx)
	if err != nil {
		return nil, err
	}
	return append(append(os.Environ(), identity...), r.Env...), nil
}

func (r *Runner) runTimeout() time.Duration {
	if r.RunTimeout > 0 {
		return r.RunTimeout
	}
	return DefaultRunTimeout
}

func (r *Runner) queryTimeout() time.Duration {
	if r.QueryTimeout > 0 {
		return r.QueryTimeout
	}
	return DefaultQueryTimeout
}

// fail wraps a subprocess error, naming the tool and — when the deadline is what ended it
// — saying so. A caller that cannot tell a timeout from a non-zero exit reports "exit
// status 1" for a tool that never exited at all.
//
// parent is the context the caller supplied; child is the deadline-bearing one this
// package derived from it. Distinguishing them separates "the operator interrupted" from
// "this call outlived its ceiling", which are different failures with different remedies.
func fail(parent, child context.Context, name string, timeout time.Duration, stderr string, err error) error {
	detail := strings.TrimSpace(stderr)
	if detail != "" {
		detail = ": " + truncate(detail)
	}
	switch {
	case parent.Err() != nil:
		return fmt.Errorf("%s: %w%s", name, parent.Err(), detail)
	case errors.Is(child.Err(), context.DeadlineExceeded):
		return fmt.Errorf("%s: no output for %s, giving up%s", name, timeout, detail)
	default:
		return fmt.Errorf("%s: %w%s", name, err, detail)
	}
}

// truncate bounds a subprocess's stderr so one runaway tool cannot flood the terminal.
// The head is kept: a tool that fails reports why on its first lines and then repeats.
func truncate(s string) string {
	const max = 2000
	if len(s) <= max {
		return s
	}
	return s[:max] + "… (truncated)"
}

// Run executes name+args, streaming output. In dry-run it prints the command.
func (r *Runner) Run(ctx context.Context, name string, args ...string) error {
	line := name + " " + strings.Join(args, " ")
	if r.DryRun {
		fmt.Fprintf(r.Out, "    → (dry-run) %s\n", line)
		return nil
	}
	fmt.Fprintf(r.Out, "    → %s\n", line)
	timeout := r.runTimeout()
	child, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	env, err := r.env(ctx)
	if err != nil {
		return fmt.Errorf("%s: resolving the identity to run as: %w", name, err)
	}
	cmd := exec.CommandContext(child, name, args...)
	cmd.Dir = r.Dir
	cmd.Env = env
	// Both streams already reach the operator, so the error carries no duplicate of them.
	cmd.Stdout = r.Out
	cmd.Stderr = r.Out
	if err := cmd.Run(); err != nil {
		return fail(ctx, child, name, timeout, "", err)
	}
	return nil
}

// Capture runs name+args and returns trimmed stdout. Returns "" in dry-run.
func (r *Runner) Capture(ctx context.Context, name string, args ...string) (string, error) {
	if r.DryRun {
		return "", nil
	}
	return r.retryRead(ctx, name, func() (string, error) { return r.captureOnce(ctx, name, args...) })
}

func (r *Runner) captureOnce(ctx context.Context, name string, args ...string) (string, error) {
	timeout := r.queryTimeout()
	child, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	env, err := r.env(ctx)
	if err != nil {
		return "", fmt.Errorf("%s: resolving the identity to run as: %w", name, err)
	}
	cmd := exec.CommandContext(child, name, args...)
	cmd.Dir = r.Dir
	cmd.Env = env
	// stderr is captured separately rather than discarded, and separately rather than
	// merged: it is the only place aws, kubectl and git say WHY a call failed, and every
	// caller here parses stdout, so merging the two would corrupt the value on the path
	// where nothing went wrong. It reaches the operator through the returned error only.
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fail(ctx, child, name, timeout, errb.String(), err)
	}
	return strings.TrimSpace(out.String()), nil
}

// readAttempts is how many times this Runner tries a read. Zero takes the default.
func (r *Runner) readAttempts() int {
	if r.ReadAttempts > 0 {
		return r.ReadAttempts
	}
	return DefaultReadAttempts
}

// retryRead runs a read up to readAttempts times, backing off between transient failures.
func (r *Runner) retryRead(ctx context.Context, name string, once func() (string, error)) (string, error) {
	var out string
	var err error
	for attempt := 1; attempt <= r.readAttempts(); attempt++ {
		out, err = once()
		if err == nil || !retryable(err) || attempt == r.readAttempts() {
			return out, err
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("%s: %w", name, ctx.Err())
		case <-time.After(backoff(attempt)):
		}
	}
	return out, err
}

// Query runs a READ-ONLY command and returns trimmed stdout, executing even in dry-run.
//
// Capture returns "" in dry-run, which is correct for a query whose answer decides what to
// CHANGE — a dry-run must not branch on live state it is only pretending to read. It is
// wrong for the enumeration half of a destructive sweep, and wrong in a way that hides the
// only question a dry-run of a sweep is asked: what would this select?
//
// A sweep that answers it with a sentence — "force-delete operator-minted IAM roles under
// /eks-agent-platform/tenants/ named <cluster>-*" — queries nothing, so it describes its
// own filter and never demonstrates it. In an account holding one estate that is a
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
// Query does NOT inherit Runner.Dir.
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
	return r.retryRead(ctx, name, func() (string, error) { return r.queryOnce(ctx, name, args...) })
}

func (r *Runner) queryOnce(ctx context.Context, name string, args ...string) (string, error) {
	timeout := r.queryTimeout()
	child, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	env, err := r.env(ctx)
	if err != nil {
		return "", fmt.Errorf("%s: resolving the identity to run as: %w", name, err)
	}
	cmd := exec.CommandContext(child, name, args...)
	cmd.Env = env
	// Captured, not discarded — the same reason Capture gives. An enumeration that fails
	// on AccessDenied and one that fails because the account is empty are the same exit
	// status, and a sweep that cannot tell them apart cannot report honestly.
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fail(ctx, child, name, timeout, errb.String(), err)
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
