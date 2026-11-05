package doctor

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// A verdict on a failed read carries kubectl's own reason, and the checks with no length
// guard no longer tick over an empty collection. Each fake below answers the way kubectl
// does: its stderr and a non-zero exit, or a document with nothing in it.

const forbidden = `echo 'Error from server (Forbidden): applications.argoproj.io is forbidden: User "x" cannot list resource' >&2; exit 1`

func notInstalledFor(kind string) string {
	return fmt.Sprintf(`echo 'error: the server doesn'"'"'t have a resource type "%s"' >&2; exit 1`, kind)
}

func wantStatus(t *testing.T, r Result, want Status, substr ...string) {
	t.Helper()
	if r.Status != want {
		t.Fatalf("%s: want %s, got %s: %s", r.Name, want, r.Status, r.Detail)
	}
	for _, s := range substr {
		if !strings.Contains(r.Detail, s) {
			t.Errorf("%s: detail is missing %q:\n%s", r.Name, s, r.Detail)
		}
	}
}

// Every read failure names what kubectl said, and none guesses a cause it never checked.
func TestReadFailuresCarryKubectlsReason(t *testing.T) {
	env := kubectlReturning(t, forbidden)
	env.Cfg.Org.GitOps.EKSGitopsRepo = "github.com/acme/eks-gitops"

	for _, r := range []Result{
		CheckGitOpsSource(context.Background(), env),
		CheckApplications(context.Background(), env),
		CheckApplicationSets(context.Background(), env),
		CheckWorkloads(context.Background(), env),
		CheckDashboards(context.Background(), env),
		CheckKarpenter(context.Background(), env),
		CheckStuckFinalizers(context.Background(), env),
	} {
		if !strings.Contains(r.Detail, "Forbidden") {
			t.Errorf("%s dropped kubectl's reason: %s", r.Name, r.Detail)
		}
		if strings.Contains(r.Detail, "is ArgoCD installed") || strings.Contains(r.Detail, "is grafana-operator installed") {
			t.Errorf("%s guesses a cause it never checked: %s", r.Name, r.Detail)
		}
		if r.Status == OK || r.Status == Skip {
			t.Errorf("%s reported %s over a read that was refused", r.Name, r.Status)
		}
	}
}

// An absent app-of-apps and an unreadable one are different findings.
func TestCheckGitOpsSource_TellsAbsentFromUnreadable(t *testing.T) {
	env := kubectlReturning(t, `echo 'Error from server (NotFound): applications.argoproj.io "app-of-apps" not found' >&2; exit 1`)
	env.Cfg.Org.GitOps.EKSGitopsRepo = "github.com/acme/eks-gitops"
	wantStatus(t, CheckGitOpsSource(context.Background(), env), Fail, "does not exist", "NotFound")

	env = kubectlReturning(t, forbidden)
	env.Cfg.Org.GitOps.EKSGitopsRepo = "github.com/acme/eks-gitops"
	wantStatus(t, CheckGitOpsSource(context.Background(), env), Fail, "could not read app-of-apps")
}

// Zero ApplicationSets is nothing examined, not every one generating cleanly.
func TestCheckApplicationSets_EmptyWarns(t *testing.T) {
	env := kubectlEnv(t, map[string]string{"applicationsets": `{"items": []}`})
	wantStatus(t, CheckApplicationSets(context.Background(), env), Warn, "nothing here was examined")
}

// Zero pods is a read that returned nothing. A provisioned platform cannot have none.
func TestCheckWorkloads_NoPodsFails(t *testing.T) {
	env := kubectlEnv(t, map[string]string{"pods": `{"items": []}`})
	wantStatus(t, CheckWorkloads(context.Background(), env), Fail, "no pods")
}

// Zero dashboards under the full tier is nothing examined.
func TestCheckDashboards_EmptyUnderTheFullTierWarns(t *testing.T) {
	env := kubectlEnv(t, map[string]string{"grafanadashboards": `{"items": []}`})
	wantStatus(t, CheckDashboards(context.Background(), env), Warn, "no GrafanaDashboards")
}

// An absent dashboard CRD under the full tier says what is missing, from kubectl's answer.
func TestCheckDashboards_MissingCRDIsNamed(t *testing.T) {
	env := kubectlReturning(t, notInstalledFor("grafanadashboards"))
	wantStatus(t, CheckDashboards(context.Background(), env), Warn, "CRD is not installed")
}

// Every dashboard rendered is still a clean pass.
func TestCheckDashboards_AllAppliedPasses(t *testing.T) {
	env := kubectlEnv(t, map[string]string{"grafanadashboards": `{"items": [
	  {"metadata": {"name": "cluster-overview"}, "status": {"conditions": [{"type": "DashboardSynchronized", "status": "True"}]}}
	]}`})
	wantStatus(t, CheckDashboards(context.Background(), env), OK, "1 applied")
}

// Only an absent resource type means Karpenter is not installed. A refused read is a read
// that did not happen, and a skip hid it.
func TestCheckKarpenter_RefusedReadWarns(t *testing.T) {
	wantStatus(t, CheckKarpenter(context.Background(), kubectlReturning(t, forbidden)), Warn, "Forbidden")
}

// A PVC read that failed leaves the commonest wedge unexamined. The old guard caught only
// the case where every kind failed.
func TestCheckStuckFinalizers_UnreadPVCWarnsAndNamesIt(t *testing.T) {
	env := kubectlReturning(t, `
case "$*" in
  *"get pvc"*) echo 'Error from server (Forbidden): persistentvolumeclaims is forbidden' >&2; exit 1 ;;
  *"get namespaces"*) echo '{"items":[]}' ;;
  *) echo 'error: the server doesn'"'"'t have a resource type "platforms"' >&2; exit 1 ;;
esac
exit 0
`)
	wantStatus(t, CheckStuckFinalizers(context.Background(), env), Warn, "pvc (", "Forbidden")
}

// A CRD that IS installed and failed to read is unread, not absent.
func TestCheckStuckFinalizers_FailedCRDReadIsNotAnAbsentCRD(t *testing.T) {
	env := kubectlReturning(t, `
case "$*" in
  *"get pvc"*|*"get namespaces"*) echo '{"items":[]}' ;;
  *) echo 'Unable to connect to the server: i/o timeout' >&2; exit 1 ;;
esac
exit 0
`)
	wantStatus(t, CheckStuckFinalizers(context.Background(), env), Warn, "platforms.platform.nanohype.dev (")
}

// A wedge found alongside an unread kind still fails, and says what it could not read.
func TestCheckStuckFinalizers_FailureNamesUnreadKinds(t *testing.T) {
	env := kubectlReturning(t, fmt.Sprintf(`
case "$*" in
  *"get pvc"*) echo '{"items":[{"metadata":{"name":"data","namespace":"app","deletionTimestamp":"%s","finalizers":["kubernetes.io/pvc-protection"]}}]}' ;;
  *"get namespaces"*) echo 'Error from server (Forbidden): namespaces is forbidden' >&2; exit 1 ;;
  *) echo 'error: the server doesn'"'"'t have a resource type "platforms"' >&2; exit 1 ;;
esac
exit 0
`, terminatingSince(stuckFor*3)))
	wantStatus(t, CheckStuckFinalizers(context.Background(), env), Fail, "pvc app/data", "namespaces (", "UNCHECKED")
}
