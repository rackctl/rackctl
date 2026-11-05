package preflight

import (
	"context"
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/doctor"
)

func quotaEnv(vcpu int, autoRequest bool) *Env {
	env := testEnv()
	env.Cfg.Quotas.VCPU = vcpu
	env.Cfg.Quotas.AutoRequest = &autoRequest
	return env
}

// A short quota with an increase already open names the request: the operator's only move
// is to wait, and the case id is what they quote to AWS support.
func TestQuota_AShortQuotaNamesThePendingRequest(t *testing.T) {
	fakeBin(t, "aws", `
case "$2" in
  get-service-quota) echo 32 ;;
  list-requested-service-quota-change-history-by-quota)
    echo '{"RequestedQuotas":[{"Id":"req-2","Status":"DENIED","DesiredValue":64},{"Id":"req-1","CaseId":"case-42","Status":"CASE_OPENED","DesiredValue":256}]}' ;;
esac`)
	r := CheckQuota(context.Background(), quotaEnv(256, true))
	mustFail(t, r, "case-42")
	if !strings.Contains(r.Detail, "CASE_OPENED") || strings.Contains(r.Detail, "req-2") {
		t.Errorf("only the open request is named, with its status:\n%s", r.Detail)
	}
}

// With autoRequest off nothing files the increase, so the remedy is the command to file it.
func TestQuota_AutoRequestOffGivesTheCommand(t *testing.T) {
	fakeBin(t, "aws", `
case "$2" in
  get-service-quota) echo 32 ;;
  list-requested-service-quota-change-history-by-quota) echo '{"RequestedQuotas":[]}' ;;
esac`)
	mustFail(t, CheckQuota(context.Background(), quotaEnv(256, false)),
		"aws service-quotas request-service-quota-increase --service-code ec2 --quota-code L-1216C47A --desired-value 256 --region us-west-2")
}

// With autoRequest on and nothing pending, the check says apply files it. It files nothing
// itself: preflight never writes.
func TestQuota_AutoRequestOnSaysApplyFilesIt(t *testing.T) {
	fakeBin(t, "aws", `
case "$2" in
  get-service-quota) echo 32 ;;
  list-requested-service-quota-change-history-by-quota) echo '{"RequestedQuotas":[]}' ;;
  *) echo "preflight must not call $2" >&2; exit 99 ;;
esac`)
	mustFail(t, CheckQuota(context.Background(), quotaEnv(256, true)), "files one before provisioning")
}

// A history that could not be read says so rather than claiming nothing is pending.
func TestQuota_AnUnreadHistoryIsSaid(t *testing.T) {
	fakeBin(t, "aws", `
case "$2" in
  get-service-quota) echo 32 ;;
  *) `+denied+` ;;
esac`)
	mustFail(t, CheckQuota(context.Background(), quotaEnv(256, true)), "could not be read")
}

// Enough quota is ok, and an unread quota is a warning, as before.
func TestQuota_SufficientAndUnread(t *testing.T) {
	fakeBin(t, "aws", `echo 512`)
	if r := CheckQuota(context.Background(), quotaEnv(256, true)); r.Status != doctor.OK {
		t.Errorf("512 holds 256: got %s %s", r.Status, r.Detail)
	}
	fakeBin(t, "aws", `echo None`)
	if r := CheckQuota(context.Background(), quotaEnv(256, true)); r.Status != doctor.Warn {
		t.Errorf("an unparseable quota is unread: got %s %s", r.Status, r.Detail)
	}
}

// Describe gives the request id always, and the case only once AWS has opened one.
func TestQuotaRequest_Describe(t *testing.T) {
	if got := (QuotaRequest{ID: "r", Status: "PENDING", DesiredValue: 256}).Describe(); got != "request r for 256 vCPU is PENDING" {
		t.Errorf("got %q", got)
	}
	if got := (QuotaRequest{ID: "r", CaseID: "c", Status: "CASE_OPENED", DesiredValue: 256}).Describe(); !strings.Contains(got, "(case c)") {
		t.Errorf("got %q", got)
	}
}
