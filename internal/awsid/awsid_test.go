package awsid

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/exec"
)

// fakeSTS puts an `aws` on PATH that answers assume-role with a session expiring
// at expiresIn from now, and logs every invocation so a test can assert on what was
// actually asked of STS — including how many times.
func fakeSTS(t *testing.T, expiresIn time.Duration) (*exec.Runner, string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "sts.log")
	exp := time.Now().Add(expiresIn).UTC().Format(time.RFC3339)

	script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %s
if [ "$2" = "assume-role" ]; then
  cat <<'JSON'
{"AccessKeyId":"ASIAFAKE","SecretAccessKey":"secret","SessionToken":"token","Expiration":"%s"}
JSON
  exit 0
fi
if [ "$2" = "get-caller-identity" ]; then
  echo "arn:aws:sts::351619759866:assumed-role/rackctl-deploy/rackctl-development"
  exit 0
fi
exit 0
`, logPath, exp)
	if err := os.WriteFile(filepath.Join(dir, "aws"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return exec.New(io.Discard), logPath
}

func stsCalls(t *testing.T, logPath string) []string {
	t.Helper()
	b, err := os.ReadFile(logPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func cfgWithRole(ar *config.AssumeRole) *config.Config {
	c := config.Default()
	c.Cloud.Profile = "stxkxs"
	c.Cloud.Region = "us-west-2"
	c.Environment = config.EnvDev
	c.Cloud.AssumeRole = ar
	return c
}

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, e := range env {
		if k, v, ok := strings.Cut(e, "="); ok {
			m[k] = v
		}
	}
	return m
}

// No role configured: unchanged behaviour, and crucially NO sts call — adding the
// seam must not make every existing config pay for a feature it does not use.
func TestEnvWithoutARoleIsTheProfile(t *testing.T) {
	run, logPath := fakeSTS(t, time.Hour)
	id := New(cfgWithRole(nil), run)

	env, err := id.Env(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m := envMap(env)
	if m["AWS_PROFILE"] != "stxkxs" || m["AWS_REGION"] != "us-west-2" {
		t.Errorf("env = %v, want the profile and region", env)
	}
	if calls := stsCalls(t, logPath); len(calls) != 0 {
		t.Errorf("no role is configured; sts must not be called.\ngot: %v", calls)
	}
}

// With a role: session credentials, the region, and NO AWS_PROFILE. A profile left
// beside explicit credentials is a second answer to the same question, and which
// one wins is a detail of whichever tool happens to be reading.
func TestEnvWithARoleIsSessionCredentialsAndNoProfile(t *testing.T) {
	run, _ := fakeSTS(t, time.Hour)
	id := New(cfgWithRole(&config.AssumeRole{RoleARN: "arn:aws:iam::351619759866:role/rackctl-deploy"}), run)

	env, err := id.Env(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m := envMap(env)
	for k, want := range map[string]string{
		"AWS_ACCESS_KEY_ID":     "ASIAFAKE",
		"AWS_SECRET_ACCESS_KEY": "secret",
		"AWS_SESSION_TOKEN":     "token",
		"AWS_REGION":            "us-west-2",
	} {
		if m[k] != want {
			t.Errorf("%s = %q, want %q", k, m[k], want)
		}
	}
	if _, ok := m["AWS_PROFILE"]; ok {
		t.Error("AWS_PROFILE must not survive alongside session credentials")
	}
}

// The session is cached. Every subprocess asks for the environment, and an sts call
// per invocation would be both slow and a throttling risk on a long apply.
func TestEnvReusesALiveSession(t *testing.T) {
	run, logPath := fakeSTS(t, time.Hour)
	id := New(cfgWithRole(&config.AssumeRole{RoleARN: "arn:aws:iam::351619759866:role/rackctl-deploy"}), run)

	for i := 0; i < 5; i++ {
		if _, err := id.Env(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(stsCalls(t, logPath)); n != 1 {
		t.Errorf("assumed %d times for 5 lookups, want 1 — the session is not being cached", n)
	}
}

// …and re-assumed when it nears expiry. A full apply builds a VPC and an EKS
// control plane and can outlive an hour; credentials that lapse mid-apply fail
// after resources are created and before state is written, which is the worst
// possible moment.
func TestEnvReassumesBeforeExpiry(t *testing.T) {
	run, logPath := fakeSTS(t, time.Hour)
	id := New(cfgWithRole(&config.AssumeRole{RoleARN: "arn:aws:iam::351619759866:role/rackctl-deploy"}), run)

	if _, err := id.Env(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Wind the clock to inside the refresh window rather than sleeping.
	id.now = func() time.Time { return time.Now().Add(time.Hour - refreshWindow + time.Minute) }
	if _, err := id.Env(context.Background()); err != nil {
		t.Fatal(err)
	}

	if n := len(stsCalls(t, logPath)); n != 2 {
		t.Errorf("assumed %d times, want 2 — a session inside the refresh window must be renewed", n)
	}
}

// The external id has to reach sts, or a cross-account trust policy that requires
// it rejects the assume — and landing-zone's own fleet-vend trust is one of those.
func TestAssumePresentsTheExternalIDAndSessionName(t *testing.T) {
	run, logPath := fakeSTS(t, time.Hour)
	id := New(cfgWithRole(&config.AssumeRole{
		RoleARN:         "arn:aws:iam::351619759866:role/rackctl-deploy",
		ExternalID:      "fleet-vend-secret",
		SessionName:     "custom-session",
		DurationSeconds: 7200,
	}), run)

	if _, err := id.Env(context.Background()); err != nil {
		t.Fatal(err)
	}
	call := strings.Join(stsCalls(t, logPath), " ")
	for _, want := range []string{
		"--external-id fleet-vend-secret",
		"--role-session-name custom-session",
		"--duration-seconds 7200",
		"--role-arn arn:aws:iam::351619759866:role/rackctl-deploy",
	} {
		if !strings.Contains(call, want) {
			t.Errorf("sts call missing %q\ngot: %s", want, call)
		}
	}
}

// The default session name names the CONFIG, not just the role. An account running
// several environments through one role otherwise shows one indistinguishable
// principal in CloudTrail, which defeats the reason for assuming a role at all.
func TestDefaultSessionNameIdentifiesTheEnvironment(t *testing.T) {
	run, logPath := fakeSTS(t, time.Hour)
	id := New(cfgWithRole(&config.AssumeRole{RoleARN: "arn:aws:iam::351619759866:role/rackctl-deploy"}), run)

	if _, err := id.Env(context.Background()); err != nil {
		t.Fatal(err)
	}
	if call := strings.Join(stsCalls(t, logPath), " "); !strings.Contains(call, "--role-session-name rackctl-development") {
		t.Errorf("default session name should identify the environment\ngot: %s", call)
	}
}

// Whoami reads the identity back from sts rather than composing it from config, so
// the banner witnesses the assume instead of restating the request.
func TestWhoamiReportsTheResolvedIdentity(t *testing.T) {
	run, _ := fakeSTS(t, time.Hour)
	id := New(cfgWithRole(&config.AssumeRole{RoleARN: "arn:aws:iam::351619759866:role/rackctl-deploy"}), run)

	got, err := id.Whoami(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "assumed-role/rackctl-deploy") {
		t.Errorf("Whoami = %q, want the assumed-role ARN sts reported", got)
	}
}

// A plan must resolve the SAME identity an apply would, or it reads the account as
// the operator and reports a plan that does not describe what apply will do.
func TestDryRunStillAssumes(t *testing.T) {
	run, logPath := fakeSTS(t, time.Hour)
	run.DryRun = true
	id := New(cfgWithRole(&config.AssumeRole{RoleARN: "arn:aws:iam::351619759866:role/rackctl-deploy"}), run)

	env, err := id.Env(context.Background())
	if err != nil {
		t.Fatalf("dry-run must still resolve the role: %v", err)
	}
	if envMap(env)["AWS_ACCESS_KEY_ID"] != "ASIAFAKE" {
		t.Errorf("dry-run env = %v, want the assumed session", env)
	}
	if len(stsCalls(t, logPath)) == 0 {
		t.Error("dry-run assumed nothing — plan would read the account as the operator")
	}
}
