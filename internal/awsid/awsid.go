// Package awsid resolves the AWS identity every rackctl subprocess runs under.
//
// rackctl orchestrates external tools — terragrunt, tofu, aws, kubectl, helm — so
// identity has to travel as environment, not as an SDK client. Two shapes:
//
//   - no assumeRole: AWS_PROFILE + AWS_REGION, as rackctl has always done.
//   - assumeRole: the profile is the SOURCE identity, rackctl assumes the role
//     from it, and every subprocess gets the resulting session credentials.
//
// The second shape exists because running as the operator's own SSO session is
// only tenable for one person on a laptop. CloudTrail attributes every write to a
// human, there is nothing to scope permissions to, and nothing to hand to CI.
//
// WHY rackctl ASSUMES THE ROLE ITSELF rather than passing an ARN downstream:
// landing-zone's live/root.hcl generates a provider with a region and default_tags
// and no assume_role block, so the roots rackctl drives have no seam to pass one
// through. (One does exist — fleet/aws/cluster-bootstrap/providers.tf — but that
// is an eks-fleet root run by provider-opentofu, not by rackctl.) Supplying
// credentials in the environment is the one mechanism that reaches terragrunt, the
// AWS CLI, and kubectl's exec plugin identically, without an upstream change and
// without writing to the operator's ~/.aws.
package awsid

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/exec"
)

// refreshWindow is how long before expiry a session is treated as spent.
//
// A single terragrunt apply can run for many minutes, and credentials that expire
// mid-apply fail somewhere arbitrary — after resources are created and before
// state is written. Re-assuming early costs one sts call and removes that class of
// failure, so the window is generous rather than tight.
const refreshWindow = 10 * time.Minute

// defaultDuration is what rackctl requests when the config does not say. It is
// also AWS's own default and the floor for a role that has not raised its
// MaxSessionDuration, so it is the value most likely to be granted as asked.
const defaultDuration = 3600

// Identity yields the AWS environment for a subprocess, re-assuming the role when
// the current session is close to expiring.
//
// The zero value is not useful; construct with New.
type Identity struct {
	cfg *config.Config
	run *exec.Runner

	mu      sync.Mutex
	creds   []string  // cached AWS_ACCESS_KEY_ID/... for the live session
	expires time.Time // when the live session lapses

	// now is time.Now, replaced in tests. Injected rather than faked globally so
	// expiry behaviour can be exercised without sleeping.
	now func() time.Time
}

// New returns an Identity for cfg. run is used only to shell out to `aws sts`, and
// must NOT carry an assumed-role environment itself — the assume starts from the
// profile, so the source identity has to be the plain one.
func New(cfg *config.Config, run *exec.Runner) *Identity {
	return &Identity{cfg: cfg, run: run, now: time.Now}
}

// Base is the identity environment with no role assumed: the profile and region.
// It is the source identity an assume starts from, and the whole answer when no
// role is configured.
func Base(cfg *config.Config) []string {
	return []string{
		"AWS_PROFILE=" + cfg.Cloud.Profile,
		"AWS_REGION=" + cfg.Cloud.Region,
	}
}

// Enabled reports whether a role is configured.
func (i *Identity) Enabled() bool {
	return i.cfg.Cloud.AssumeRole != nil && i.cfg.Cloud.AssumeRole.RoleARN != ""
}

// Env returns the environment a subprocess should run with.
//
// Without a configured role this is Base. With one, it is the assumed session's
// credentials plus the region — and NOT AWS_PROFILE, because a profile left in the
// environment alongside explicit credentials is a second answer to the same
// question, and which one wins is a detail of whichever tool is reading it.
//
// Safe to call per invocation; the session is cached and re-assumed only when it
// nears expiry.
func (i *Identity) Env(ctx context.Context) ([]string, error) {
	if !i.Enabled() {
		return Base(i.cfg), nil
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	if i.creds != nil && i.now().Before(i.expires.Add(-refreshWindow)) {
		return i.creds, nil
	}
	creds, expires, err := i.assume(ctx)
	if err != nil {
		return nil, err
	}
	i.creds, i.expires = creds, expires
	return creds, nil
}

// stsCredentials is the shape of `aws sts assume-role --query Credentials`.
type stsCredentials struct {
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	SessionToken    string `json:"SessionToken"`
	Expiration      string `json:"Expiration"`
}

func (i *Identity) assume(ctx context.Context) ([]string, time.Time, error) {
	ar := i.cfg.Cloud.AssumeRole

	dur := ar.DurationSeconds
	if dur == 0 {
		dur = defaultDuration
	}
	name := ar.SessionName
	if name == "" {
		// Say which config did the work, not just which role — an account with
		// several environments otherwise shows one indistinguishable principal.
		name = "rackctl-" + string(i.cfg.Environment)
	}

	args := []string{"sts", "assume-role",
		"--role-arn", ar.RoleARN,
		"--role-session-name", name,
		"--duration-seconds", fmt.Sprint(dur),
		"--query", "Credentials",
		"--output", "json",
	}
	if ar.ExternalID != "" {
		args = append(args, "--external-id", ar.ExternalID)
	}

	// Deliberately the BASE environment: the assume is performed by the source
	// identity. Handing it the previous session's credentials would chain a role
	// off itself, which fails once the chain exceeds one hop and is not what the
	// config asked for anyway.
	src := *i.run
	src.Env = Base(i.cfg)

	// Query, not Capture: Capture returns "" under dry-run, which would leave a
	// `rackctl plan` unable to assume and therefore planning as the WRONG identity —
	// its read-only AWS calls would answer for the operator rather than for the role.
	// Assuming a role creates no infrastructure; it is exactly the read-only-but-must-
	// still-run case Query exists for.
	out, err := src.Query(ctx, "aws", args...)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf(
			"assuming %s as profile %q: %w\n"+
				"Check the role's trust policy admits that profile's principal%s, and that the "+
				"SSO session has not expired (`aws sso login --profile %s`)",
			ar.RoleARN, i.cfg.Cloud.Profile, err,
			externalIDHint(ar.ExternalID), i.cfg.Cloud.Profile)
	}

	var c stsCredentials
	if err := json.Unmarshal([]byte(out), &c); err != nil {
		return nil, time.Time{}, fmt.Errorf("reading the assumed session for %s: %w", ar.RoleARN, err)
	}
	if c.AccessKeyID == "" || c.SecretAccessKey == "" || c.SessionToken == "" {
		return nil, time.Time{}, fmt.Errorf("sts returned an incomplete session for %s", ar.RoleARN)
	}

	// A session whose expiry cannot be read is treated as expiring now, so the
	// next call re-assumes. Better a redundant sts call than credentials that
	// silently outlive their window.
	expires, err := time.Parse(time.RFC3339, c.Expiration)
	if err != nil {
		expires = i.now()
	}

	return []string{
		"AWS_ACCESS_KEY_ID=" + c.AccessKeyID,
		"AWS_SECRET_ACCESS_KEY=" + c.SecretAccessKey,
		"AWS_SESSION_TOKEN=" + c.SessionToken,
		"AWS_REGION=" + i.cfg.Cloud.Region,
	}, expires, nil
}

func externalIDHint(id string) string {
	if id == "" {
		return " and requires no sts:ExternalId (cloud.assumeRole.externalId is unset)"
	}
	return " and matches the sts:ExternalId being presented"
}

// Whoami returns the ARN the current identity actually resolves to.
//
// Read from sts, not composed from config: the point is to WITNESS the identity
// rather than restate what was requested. A banner built from config says what
// rackctl meant to be; this says what it is.
func (i *Identity) Whoami(ctx context.Context) (string, error) {
	env, err := i.Env(ctx)
	if err != nil {
		return "", err
	}
	q := *i.run
	q.Env = env
	out, err := q.Query(ctx, "aws", "sts", "get-caller-identity", "--query", "Arn", "--output", "text")
	if err != nil {
		return "", err
	}
	return out, nil
}
