package preflight

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/doctor"
	"github.com/rackctl/rackctl/internal/exec"
)

// fakeBin puts an executable named `name` on PATH whose body is `body`, a /bin/sh
// script receiving the real arguments. The Runner shells out by name, so this is the
// seam — the tests exercise the actual argument-building and output-parsing, not a stub
// of it. A check that is never proven to FAIL is a check that cannot be trusted to
// catch anything.
func fakeBin(t *testing.T, name, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func testEnv() *Env {
	return &Env{
		Cfg: &config.Config{
			Org:         config.Org{Name: "acme"},
			Cloud:       config.Cloud{AccountID: "111122223333", Region: "us-west-2", Profile: "acme"},
			Environment: config.EnvDev,
			Cluster:     config.Cluster{Name: "platform"},
		},
		Run: exec.New(io.Discard),
	}
}

func mustFail(t *testing.T, r doctor.Result, wantSubstr string) {
	t.Helper()
	if r.Status != doctor.Fail {
		t.Fatalf("%s: want Fail, got %s — a check that cannot fail catches nothing.\ndetail: %s",
			r.Name, r.Status, r.Detail)
	}
	if wantSubstr != "" && !strings.Contains(r.Detail, wantSubstr) {
		t.Errorf("%s: detail does not name the remedy (%q missing):\n%s", r.Name, wantSubstr, r.Detail)
	}
}

// ─────────────────────────── registration ───────────────────────────

// A check that is not in Run()'s list never executes, however good it is. Nothing else
// asserts that list, so a check could be written, tested, and silently never run.
func TestRun_EveryCheckIsRegistered(t *testing.T) {
	fakeBin(t, "aws", `exit 1`)
	fakeBin(t, "gh", `exit 1`)
	t.Setenv("GITHUB_TOKEN", "")

	var names []string
	for _, r := range Run(context.Background(), testEnv()) {
		names = append(names, r.Name)
	}
	for _, want := range []string{
		"aws identity", "session lifetime", "vcpu quota", "terraform state",
		"orphan collisions", "bucket names", "hosted zone", "bedrock logging",
		"soft-deleted secrets", "cost allocation", "catalog fork", "github token",
		"local vend",
	} {
		if !slices.Contains(names, want) {
			t.Errorf("check %q is not registered in Run() — it would never execute; got %v", want, names)
		}
	}
}

// ─────────────────────────── github credential ───────────────────────────

// tokenEnv is testEnv with a tenants repo set — the setting that arms cluster-bootstrap's
// github provider — and any real GITHUB_TOKEN cleared so the test controls both channels.
func tokenEnv(t *testing.T, repo string) *Env {
	t.Helper()
	t.Setenv("GITHUB_TOKEN", "")
	e := testEnv()
	e.Cfg.Org.GitOps.TenantsRepo = repo
	return e
}

// The failure this exists to prevent: a 401 in PHASE 5, after the VPC and the EKS cluster
// are built and billing. Knowable in a second, before a dollar is spent.
func TestCheckGitHubToken_FailsWhenTheConfigNeedsATokenAndNoneExists(t *testing.T) {
	fakeBin(t, "gh", `exit 1`)

	r := CheckGitHubToken(context.Background(), tokenEnv(t, "github.com/acme/tenants"))
	mustFail(t, r, "gh auth login")
	if !strings.Contains(r.Detail, "phase 5") {
		t.Errorf("must say WHEN it would blow up — that is the whole cost argument:\n%s", r.Detail)
	}
}

// `gh auth login` stores the credential in gh's keyring and exports nothing. rackctl
// bridges it, so this is a healthy state rather than the failure it used to be.
func TestCheckGitHubToken_OKWhenGhHoldsOne(t *testing.T) {
	fakeBin(t, "gh", `[ "$1 $2" = "auth token" ] && echo ghs_x; exit 0`)

	if r := CheckGitHubToken(context.Background(), tokenEnv(t, "github.com/acme/tenants")); r.Status != doctor.OK {
		t.Fatalf("gh holding a token is sufficient — rackctl passes it through: %s", r.Detail)
	}
}

// A zero exit with no output is not a credential. Reading it as one puts the check right
// back to green-lighting the 401 it was written to catch.
func TestCheckGitHubToken_ZeroExitWithNoTokenIsNotHealthy(t *testing.T) {
	fakeBin(t, "gh", `exit 0`)

	if r := CheckGitHubToken(context.Background(), tokenEnv(t, "github.com/acme/tenants")); r.Status == doctor.OK {
		t.Fatalf("an empty token must never report OK:\n%s", r.Detail)
	}
}

// No tenants repo means the provider is never called. Gating an install on a credential
// it will never use would be a false alarm on the common path.
func TestCheckGitHubToken_OKWhenNoTenantsRepo(t *testing.T) {
	fakeBin(t, "gh", `exit 1`)

	if r := CheckGitHubToken(context.Background(), tokenEnv(t, "")); r.Status != doctor.OK {
		t.Fatalf("portal-off installs need no GitHub token: %s", r.Detail)
	}
}

// ─────────────────────────── stale state ───────────────────────────

// State that outlives its cluster is the failure that ended a session: the cluster was
// deleted out of band, state still claimed 90 resources, and every component would have
// reconciled against things that were not there.
func TestCheckStaleState_FailsWhenStateOutlivesTheCluster(t *testing.T) {
	fakeBin(t, "aws", `
case "$1 $2" in
  "eks describe-cluster") exit 1 ;;                        # the cluster is GONE
  "s3 cp")                echo '{"resources":[{"type":"aws_vpc"},{"type":"aws_subnet"}]}' ;;
  *)                      echo "" ;;
esac
exit 0`)

	r := CheckStaleState(context.Background(), testEnv())
	mustFail(t, r, "rackctl destroy --apply")
}

// The remedy must be DESTROY-then-maybe-purge, never purge-first.
//
// The first real run proved why: a rollback destroyed the cluster and the VPC, but its
// teardown of secrets and agent-iam failed — so their state was entirely ACCURATE. An
// IAM role, two S3 buckets and a KMS key were all still live. An earlier draft of this
// check inferred "cluster gone ⇒ state stale" and advised purging the state, which would
// have orphaned every one of them permanently — the exact failure preflight exists to
// prevent. A missing cluster proves the CLUSTER is gone; it proves nothing about what
// the other components own.
func TestCheckStaleState_NeverAdvisesPurgingStateThatMayTrackLiveResources(t *testing.T) {
	fakeBin(t, "aws", `
case "$1 $2" in
  "eks describe-cluster") exit 1 ;;
  "s3 cp")                echo '{"resources":[{"type":"aws_iam_role"},{"type":"aws_s3_bucket"}]}' ;;
  *)                      echo "" ;;
esac
exit 0`)

	r := CheckStaleState(context.Background(), testEnv())
	mustFail(t, r, "")

	if !strings.Contains(r.Detail, "ONLY") {
		t.Errorf("purging must be conditional, not the headline remedy:\n%s", r.Detail)
	}
	if !strings.Contains(r.Detail, "orphans them permanently") {
		t.Errorf("must warn that purging live-tracking state orphans the resources:\n%s", r.Detail)
	}
	// The dangerous advice: purge, stated unconditionally, ahead of destroy.
	purge := strings.Index(r.Detail, "Purge")
	destroy := strings.Index(r.Detail, "rackctl destroy")
	if destroy == -1 || (purge != -1 && purge < destroy) {
		t.Errorf("destroy must be offered BEFORE purge — purge-first orphans live resources:\n%s", r.Detail)
	}
}

// The same state is correct when the cluster exists — that is a re-apply, not a bug.
// Without this, preflight would block every legitimate update.
func TestCheckStaleState_OKWhenTheClusterIsLive(t *testing.T) {
	fakeBin(t, "aws", `
case "$1 $2" in
  "eks describe-cluster") echo ACTIVE ;;
  "s3 cp")                echo '{"resources":[{"type":"aws_vpc"}]}' ;;
  *)                      echo "" ;;
esac
exit 0`)

	if r := CheckStaleState(context.Background(), testEnv()); r.Status != doctor.OK {
		t.Fatalf("a live cluster with state is a re-apply, not a failure: %s — %s", r.Status, r.Detail)
	}
}

// ─────────────────────────── collisions ───────────────────────────

// The KMS alias is the one that cannot be retried out of: scheduling a key for deletion
// does not free its alias, so the next install dies on AliasAlreadyExists against a key
// that can no longer be revived.
func TestCheckCollisions_FailsOnOrphanedKMSAlias(t *testing.T) {
	fakeBin(t, "aws", `
case "$1 $2" in
  "eks describe-cluster") exit 1 ;;   # no cluster ⇒ anything left over is an orphan
  "kms list-aliases")     echo 1 ;;
  *)                      echo 0 ;;
esac
exit 0`)

	r := CheckCollisions(context.Background(), testEnv())
	mustFail(t, r, "alias/eks/development-platform")
	if !strings.Contains(r.Detail, "does NOT free its alias") {
		t.Errorf("must name the alias trap explicitly — it is the non-obvious half:\n%s", r.Detail)
	}
}

// A live cluster owns these resources; they are not orphans. Guards against preflight
// refusing to run against a healthy platform.
func TestCheckCollisions_OKWhenTheClusterIsLive(t *testing.T) {
	fakeBin(t, "aws", `
case "$1 $2" in
  "eks describe-cluster") echo ACTIVE ;;
  *)                      echo 1 ;;   # everything "exists" — but it belongs to the cluster
esac
exit 0`)

	if r := CheckCollisions(context.Background(), testEnv()); r.Status != doctor.OK {
		t.Fatalf("a live cluster's own resources are not collisions: %s — %s", r.Status, r.Detail)
	}
}

// ─────────────────────────── soft-deleted secrets ───────────────────────────

// A soft-deleted secret holds its NAME for the whole recovery window, so terraform
// cannot recreate it — and the error says nothing about a recovery window. The name checked
// is the cluster-keyed one managed-monitoring actually creates (<cluster>-grafana-token).
func TestCheckSoftDeletedSecrets_FailsOnAPendingDeletion(t *testing.T) {
	fakeBin(t, "aws", `echo "development-platform-grafana-token"`)

	r := CheckSoftDeletedSecrets(context.Background(), testEnv())
	mustFail(t, r, "--force-delete-without-recovery")
}

func TestCheckSoftDeletedSecrets_IgnoresUnrelatedPendingSecrets(t *testing.T) {
	fakeBin(t, "aws", `echo "some-other-teams-secret"`)

	if r := CheckSoftDeletedSecrets(context.Background(), testEnv()); r.Status != doctor.OK {
		t.Fatalf("only the names the platform creates can block it: %s — %s", r.Status, r.Detail)
	}
}

// The names are DERIVED from the cluster name, not pinned literals. managed-monitoring keys
// both secrets on the full cluster name (<environment>-<base>-...), so a hardcoded name
// stops matching the moment the cluster is named anything else — and the check then passes
// vacuously against a substrate whose secrets it never inspected. Two things must hold: the
// pre-rename literal (eks-grafana-token) is NOT one of this cluster's names, and a cluster
// named differently is tracked under its own name.
func TestCheckSoftDeletedSecrets_DerivesNamesFromClusterNotStaleLiteral(t *testing.T) {
	// The pre-rename literal is not a name development-platform creates — must not be flagged.
	fakeBin(t, "aws", `echo "eks-grafana-token"`)
	if r := CheckSoftDeletedSecrets(context.Background(), testEnv()); r.Status != doctor.OK {
		t.Fatalf("the stale literal is not a name this cluster creates — it must not block: %s — %s", r.Status, r.Detail)
	}

	// A differently named cluster keys its secrets differently; the check must follow.
	env := testEnv()
	env.Cfg.Cluster.Name = "apex"
	fakeBin(t, "aws", `echo "development-apex-managed-monitoring-endpoints"`)
	r := CheckSoftDeletedSecrets(context.Background(), env)
	mustFail(t, r, "development-apex-managed-monitoring-endpoints")
}

// ─────────────────────────── catalog fork ───────────────────────────

// The cluster reads its catalog from the FORK. A fork behind upstream silently builds a
// cluster without the fix the run was meant to prove.
func TestCheckCatalogFork_FailsWhenTheForkIsBehind(t *testing.T) {
	fakeBin(t, "gh", `
if [ "$1" = "api" ]; then echo 3; fi
exit 0`)

	r := CheckCatalogFork(context.Background(), testEnv())
	mustFail(t, r, "gh repo sync")
	if !strings.Contains(r.Detail, "reads its catalog from the FORK") {
		t.Errorf("must explain why being behind is silent:\n%s", r.Detail)
	}
}

// An unreadable response must NOT read as healthy.
//
// This test exists because the first draft did `behind, _ := strconv.Atoi(out)` — so a
// garbled response parsed to zero and the check reported the fork as CURRENT. A
// preflight that answers "fine" when it cannot tell is worse than no preflight: it is
// the same green-light-that-means-nothing the command was written to eliminate. Found
// only because a fake `gh` echoed twice.
func TestCheckCatalogFork_UnreadableResponseIsNotHealthy(t *testing.T) {
	fakeBin(t, "gh", `
if [ "$1" = "api" ]; then echo "not-a-number"; fi
exit 0`)

	r := CheckCatalogFork(context.Background(), testEnv())
	if r.Status == doctor.OK {
		t.Fatalf("a response the check cannot parse must never report OK — that is a green "+
			"light that means nothing.\ndetail: %s", r.Detail)
	}
}

// A fork that is level with upstream is fine. (A fork AHEAD is also fine — the org owns
// it — which is why the check reads ahead_by of upstream-vs-fork, not a raw inequality.)
func TestCheckCatalogFork_OKWhenLevel(t *testing.T) {
	fakeBin(t, "gh", `
if [ "$1" = "api" ]; then echo 0; fi
exit 0`)

	if r := CheckCatalogFork(context.Background(), testEnv()); r.Status != doctor.OK {
		t.Fatalf("a fork level with upstream is fine: %s — %s", r.Status, r.Detail)
	}
}

// A fork that does not exist yet is not a failure — init will create it.
func TestCheckCatalogFork_OKWhenForkDoesNotExistYet(t *testing.T) {
	fakeBin(t, "gh", `exit 1`)

	if r := CheckCatalogFork(context.Background(), testEnv()); r.Status != doctor.OK {
		t.Fatalf("a missing fork is init's job, not a preflight failure: %s — %s", r.Status, r.Detail)
	}
}

// ─────────────────────────── identity ───────────────────────────

// The profile is ambient; the account id is declared. Nothing compared them, so a
// mismatch would build a complete, healthy platform in the wrong account.
func TestCheckIdentity_FailsOnAccountMismatch(t *testing.T) {
	fakeBin(t, "aws", `echo 999988887777`)

	r := CheckIdentity(context.Background(), testEnv())
	mustFail(t, r, "wrong account")
}

func TestCheckIdentity_OKWhenAccountMatches(t *testing.T) {
	fakeBin(t, "aws", `echo 111122223333`)

	if r := CheckIdentity(context.Background(), testEnv()); r.Status != doctor.OK {
		t.Fatalf("matching account must pass: %s — %s", r.Status, r.Detail)
	}
}
