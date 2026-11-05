package cmd

import (
	"context"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/doctor"
)

// fakeQuotaAWS puts an aws on PATH whose credentials are for the account quotaCfg names,
// answers the vCPU quota with have, lists pending as the open request (or none), and records
// a filed request so the history lists it after. It returns a reader for every invocation.
func fakeQuotaAWS(t *testing.T, have, pending string) func() string {
	t.Helper()
	return fakeQuotaAWSFor(t, "111111111111", have, pending)
}

// fakeQuotaAWSFor is fakeQuotaAWS with the credentials' account named.
func fakeQuotaAWSFor(t *testing.T, account, have, pending string) func() string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	filed := filepath.Join(dir, "filed")
	script := `#!/bin/sh
echo "$*" >> ` + log + `
case "$2" in
  get-caller-identity) echo "` + account + `" ;;
  get-service-quota) echo "` + have + `" ;;
  list-requested-service-quota-change-history-by-quota)
    if [ -f ` + filed + ` ]; then
      echo '{"RequestedQuotas":[{"Id":"req-filed","CaseId":"","Status":"PENDING","DesiredValue":256}]}'
    else
      echo '` + pending + `'
    fi ;;
  request-service-quota-increase) : > ` + filed + ` ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "aws"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() string {
		b, _ := os.ReadFile(log)
		return string(b)
	}
}

func quotaCfg() *config.Config {
	cfg := config.Default()
	cfg.Cloud.AccountID = "111111111111"
	cfg.Cloud.Region = "us-west-2"
	return cfg
}

const noPending = `{"RequestedQuotas":[{"Id":"req-old","CaseId":"case-old","Status":"APPROVED","DesiredValue":64}]}`

// A fresh account's quota is short and nothing has been filed: the apply files the increase
// before the gate and refuses with the request named, as refused. Before this step the gate
// refused for want of a request that only phase 0, after the gate, would ever file.
func TestRequestQuota_FilesTheIncreaseAndRefusesNamingIt(t *testing.T) {
	calls := fakeQuotaAWS(t, "32", noPending)
	err := requestQuota(context.Background(), quotaCfg(), nil, io.Discard)
	if ExitCode(err) != ExitRefused {
		t.Fatalf("exit = %d (%v), want %d refused — nothing was provisioned", ExitCode(err), err, ExitRefused)
	}
	if !strings.Contains(calls(), "request-service-quota-increase") {
		t.Fatalf("the increase was never filed:\n%s", calls())
	}
	if !strings.Contains(err.Error(), "req-filed") {
		t.Errorf("the refusal must name the request it filed: %v", err)
	}
}

// Credentials for another account file nothing there, and leave the refusal to the gate's
// identity check. The step ran ahead of that check and filed the increase in whatever account
// the ambient profile resolved to, then refused over the stray request on every re-run.
func TestRequestQuota_CredentialsForAnotherAccountFileNothing(t *testing.T) {
	calls := fakeQuotaAWSFor(t, "222222222222", "32", noPending)
	if err := requestQuota(context.Background(), quotaCfg(), nil, io.Discard); err != nil {
		t.Fatalf("an account mismatch is the gate's refusal to give, got %v", err)
	}
	if strings.Contains(calls(), "request-service-quota-increase") {
		t.Errorf("nothing may be filed in an account the config does not name:\n%s", calls())
	}
}

// A pending request in another account is not named as this config's: the refusal over it
// hid the mismatch.
func TestRequestQuota_APendingRequestInAnotherAccountIsNotNamed(t *testing.T) {
	fakeQuotaAWSFor(t, "222222222222", "32",
		`{"RequestedQuotas":[{"Id":"req-1","CaseId":"case-42","Status":"CASE_OPENED","DesiredValue":256}]}`)
	if err := requestQuota(context.Background(), quotaCfg(), nil, io.Discard); err != nil {
		t.Fatalf("an account mismatch is the gate's refusal to give, got %v", err)
	}
}

// An identity that could not be read files nothing.
func TestRequestQuota_AnUnreadIdentityFilesNothing(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	script := `#!/bin/sh
echo "$*" >> ` + log + `
case "$2" in
  get-caller-identity) echo "An error occurred (ExpiredToken)" >&2; exit 254 ;;
  get-service-quota) echo 32 ;;
  list-requested-service-quota-change-history-by-quota) echo '{"RequestedQuotas":[]}' ;;
  *) : ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "aws"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := requestQuota(context.Background(), quotaCfg(), nil, io.Discard); err != nil {
		t.Fatalf("an unread identity is the gate's refusal to give, got %v", err)
	}
	b, _ := os.ReadFile(log)
	if strings.Contains(string(b), "request-service-quota-increase") {
		t.Errorf("nothing may be filed under credentials not matched to the config:\n%s", b)
	}
}

// A request already open is named, and not filed a second time.
func TestRequestQuota_APendingRequestIsNamedNotRefiled(t *testing.T) {
	calls := fakeQuotaAWS(t, "32",
		`{"RequestedQuotas":[{"Id":"req-1","CaseId":"case-42","Status":"CASE_OPENED","DesiredValue":256}]}`)
	err := requestQuota(context.Background(), quotaCfg(), nil, io.Discard)
	if ExitCode(err) != ExitRefused {
		t.Fatalf("exit = %d, want %d refused", ExitCode(err), ExitRefused)
	}
	if strings.Contains(calls(), "request-service-quota-increase") {
		t.Errorf("a pending request must not be filed again:\n%s", calls())
	}
	if !strings.Contains(err.Error(), "case-42") || !strings.Contains(err.Error(), "CASE_OPENED") {
		t.Errorf("the refusal must name the case and its status: %v", err)
	}
}

// A quota that already holds the config files nothing and refuses nothing.
func TestRequestQuota_ASufficientQuotaPasses(t *testing.T) {
	calls := fakeQuotaAWS(t, "512", noPending)
	if err := requestQuota(context.Background(), quotaCfg(), nil, io.Discard); err != nil {
		t.Fatalf("a sufficient quota must pass: %v", err)
	}
	if strings.Contains(calls(), "request-service-quota-increase") {
		t.Errorf("nothing should be filed:\n%s", calls())
	}
}

// autoRequest off files nothing: the gate refuses and names the command.
func TestRequestQuota_AutoRequestOffFilesNothing(t *testing.T) {
	calls := fakeQuotaAWS(t, "32", noPending)
	cfg := quotaCfg()
	off := false
	cfg.Quotas.AutoRequest = &off
	if err := requestQuota(context.Background(), cfg, nil, io.Discard); err != nil {
		t.Fatalf("with autoRequest off the gate decides, got %v", err)
	}
	if calls() != "" {
		t.Errorf("nothing should be asked of AWS:\n%s", calls())
	}
}

// A quota that could not be read is the gate's warning to give, not a refusal here.
func TestRequestQuota_AnUnreadQuotaIsLeftToTheGate(t *testing.T) {
	calls := fakeQuotaAWS(t, "not-a-number", noPending)
	if err := requestQuota(context.Background(), quotaCfg(), nil, io.Discard); err != nil {
		t.Fatalf("an unread quota must be left to the gate: %v", err)
	}
	if strings.Contains(calls(), "request-service-quota-increase") {
		t.Errorf("nothing should be filed over an unread quota:\n%s", calls())
	}
}

// A history that could not be read files nothing: a blind request could duplicate one open.
func TestRequestQuota_AnUnreadHistoryFilesNothing(t *testing.T) {
	calls := fakeQuotaAWS(t, "32", "not json")
	if err := requestQuota(context.Background(), quotaCfg(), nil, io.Discard); err != nil {
		t.Fatalf("an unread history must be left to the gate: %v", err)
	}
	if strings.Contains(calls(), "request-service-quota-increase") {
		t.Errorf("nothing should be filed blind:\n%s", calls())
	}
}

// A filing AWS rejected is still a refusal before spend, and names the command to file it.
func TestRequestQuota_AFailedFilingRefusesWithTheCommand(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
case "$2" in
  get-caller-identity) echo 111111111111 ;;
  get-service-quota) echo 32 ;;
  list-requested-service-quota-change-history-by-quota) echo '{"RequestedQuotas":[]}' ;;
  *) echo "An error occurred (AccessDeniedException)" >&2; exit 254 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "aws"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	err := requestQuota(context.Background(), quotaCfg(), nil, io.Discard)
	if ExitCode(err) != ExitRefused {
		t.Fatalf("exit = %d, want %d refused", ExitCode(err), ExitRefused)
	}
	if !strings.Contains(err.Error(), "request-service-quota-increase --service-code ec2") {
		t.Errorf("the refusal must give the command to file it by hand: %v", err)
	}
}

// captureStdout runs f with os.Stdout redirected and returns what it printed.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	f()
	_ = w.Close()
	os.Stdout = old
	return <-done
}

// A gate that passed with warnings shows them. It printed only "preflight clear", so a check
// that said it was never examined went unseen on the one path that spends.
func TestPreflightGate_ShowsWarningsAndSkipsWhenItPasses(t *testing.T) {
	var err error
	out := captureStdout(t, func() {
		err = reportPreflightGate([]doctor.Result{
			{Name: "aws identity", Status: doctor.OK, Detail: "matches"},
			{Name: "version skew", Status: doctor.Warn, Detail: "skew is UNCHECKED"},
			{Name: "github token", Status: doctor.Skip, Detail: "portal off"},
		})
	})
	if err != nil {
		t.Fatalf("warnings never refuse: %v", err)
	}
	for _, want := range []string{"1 warning(s), 1 skipped", "skew is UNCHECKED", "portal off"} {
		if !strings.Contains(out, want) {
			t.Errorf("the gate output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "matches") {
		t.Errorf("an ok row is not shown when the gate passes:\n%s", out)
	}
	if strings.Contains(out, "preflight clear") {
		t.Errorf("a gate with warnings is not clear:\n%s", out)
	}
}

// Every check ok stays one quiet line.
func TestPreflightGate_IsQuietWhenEveryCheckIsOK(t *testing.T) {
	out := captureStdout(t, func() {
		if err := reportPreflightGate([]doctor.Result{{Name: "aws identity", Status: doctor.OK, Detail: "matches"}}); err != nil {
			t.Errorf("an ok gate refused: %v", err)
		}
	})
	if !strings.Contains(out, "preflight clear") || strings.Contains(out, "matches") {
		t.Errorf("an all-ok gate prints one line:\n%s", out)
	}
}

// A failure still refuses and prints every row.
func TestPreflightGate_RefusesOnAFailure(t *testing.T) {
	out := captureStdout(t, func() {
		if err := reportPreflightGate([]doctor.Result{
			{Name: "aws identity", Status: doctor.OK, Detail: "matches"},
			{Name: "vcpu quota", Status: doctor.Fail, Detail: "32 vCPU"},
		}); err == nil {
			t.Error("a failed gate must refuse")
		}
	})
	if !strings.Contains(out, "matches") || !strings.Contains(out, "32 vCPU") {
		t.Errorf("a failed gate prints every row:\n%s", out)
	}
}

// Through the binary: an apply on a short quota files the increase before the gate and
// exits refused, before any phase runs.
func TestApply_AShortQuotaFilesAndExitsRefusedBeforeAnyPhase(t *testing.T) {
	bin := buildRackctl(t)
	calls := fakeQuotaAWS(t, "32", noPending)
	cfg := filepath.Join(t.TempDir(), "rackctl.yaml")
	if err := os.WriteFile(cfg, []byte(`org:
  name: acme
cloud:
  provider: aws
  accountId: "111111111111"
  region: us-west-2
  profile: test
environment: development
cluster:
  name: platform
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := osexec.Command(bin, "apply", "-c", cfg)
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if got := exitStatus(err); got != ExitRefused {
		t.Fatalf("exit = %d, want %d refused:\n%s", got, ExitRefused, out)
	}
	if !strings.Contains(calls(), "request-service-quota-increase") {
		t.Fatalf("the increase was not filed:\n%s", calls())
	}
	if strings.Contains(string(out), "Preflight — tools") {
		t.Errorf("no phase may run before the quota is granted:\n%s", out)
	}
}

// Through the binary: credentials for another account file nothing, and the apply exits
// preflight with the mismatch named rather than refused over a quota in the wrong account.
func TestApply_CredentialsForAnotherAccountFileNoQuotaAndExitPreflight(t *testing.T) {
	bin := buildRackctl(t)
	calls := fakeQuotaAWSFor(t, "222222222222", "32", noPending)
	cfg := filepath.Join(t.TempDir(), "rackctl.yaml")
	if err := os.WriteFile(cfg, []byte(`org:
  name: acme
cloud:
  provider: aws
  accountId: "111111111111"
  region: us-west-2
  profile: test
environment: development
cluster:
  name: platform
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := osexec.Command(bin, "apply", "-c", cfg)
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if got := exitStatus(err); got != ExitPreflight {
		t.Fatalf("exit = %d, want %d preflight:\n%s", got, ExitPreflight, out)
	}
	if strings.Contains(calls(), "request-service-quota-increase") {
		t.Fatalf("the increase was filed in an account the config does not name:\n%s", calls())
	}
	if !strings.Contains(string(out), "credentials are for account 222222222222") {
		t.Errorf("the gate must name the account mismatch:\n%s", out)
	}
}
