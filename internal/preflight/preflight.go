// Package preflight answers one question before a single dollar is spent: can this
// install actually succeed?
//
// # WHY THIS EXISTS
//
// `doctor` inspects a PROVISIONED platform. It needs a live cluster, so it can only
// tell you why an install broke — never that it was going to. Every failure of the
// first four provisioning runs was knowable in advance and cost a full run to find:
//
//   - `BucketAlreadyExists` on a bucket name that is globally unique across every AWS
//     account on earth. Unrecoverable by retry. Discovered 6 minutes in.
//   - `ResourceInUseException` — two components claiming Pod Identity for one service
//     account. A service account can hold exactly one association.
//   - A `gh repo fork` 403 that made `init` permanently un-rerunnable.
//   - Terraform state that still claimed 90 resources after the cloud was emptied out
//     of band. Every component reconciles against things that are not there.
//   - A KMS key scheduled for deletion whose ALIAS was left behind — the next install
//     dies on AliasAlreadyExists against a key that cannot be revived.
//   - A catalog fork one commit behind upstream, which would have built a cluster
//     missing the very fix the run was meant to prove.
//
// None of these are cloud failures. They are collisions with the wreckage of a previous
// attempt, and a machine can enumerate them in seconds.
//
// # THE RULE
//
// A check belongs here if it is (a) knowable without a cluster and (b) fatal or
// misleading if left until apply-time. It must be read-only: preflight never fixes what
// it finds, because a tool that mutates the account it is auditing cannot be trusted to
// audit it. It names the remedy and exits non-zero.
package preflight

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/doctor"
	"github.com/rackctl/rackctl/internal/engine"
	"github.com/rackctl/rackctl/internal/exec"
	"github.com/rackctl/rackctl/internal/phases"
)

// Env is what a check gets to work with. Deliberately the same shape as doctor.Env —
// the two commands differ in WHEN they run, not in what they are.
type Env struct {
	Cfg *config.Config
	Run *exec.Runner
}

func ok(n, d string) doctor.Result   { return doctor.Result{Name: n, Status: doctor.OK, Detail: d} }
func warn(n, d string) doctor.Result { return doctor.Result{Name: n, Status: doctor.Warn, Detail: d} }
func fail(n, d string) doctor.Result { return doctor.Result{Name: n, Status: doctor.Fail, Detail: d} }

// Run executes every check and returns results in a stable order. Like doctor, it does
// not stop at the first failure: the whole point is to hand back the complete list of
// what must be cleared, so it can be cleared in one pass rather than one run per bug.
func Run(ctx context.Context, env *Env) []doctor.Result {
	return []doctor.Result{
		CheckIdentity(ctx, env),
		CheckSessionLifetime(ctx, env),
		CheckQuota(ctx, env),
		CheckVersionSkew(ctx, env),
		CheckStaleState(ctx, env),
		CheckCollisions(ctx, env),
		CheckBucketNames(ctx, env),
		CheckHostedZone(ctx, env),
		CheckBedrockLogging(ctx, env),
		CheckSoftDeletedSecrets(ctx, env),
		CheckCostAllocationTags(ctx, env),
		CheckCatalogFork(ctx, env),
		CheckCostPipelinePrerequisite(ctx, env),
		CheckGitHubToken(ctx, env),
		CheckVendFreshness(ctx, env),
	}
}

// Failed reports whether any result is a Fail. Re-exported so callers need not import
// doctor just to read a preflight verdict.
func Failed(rs []doctor.Result) bool { return doctor.Failed(rs) }

// clusterName is the name every component derives its resources from.
func clusterName(cfg *config.Config) string { return cfg.ClusterName() }

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// aws runs an aws CLI query and returns trimmed stdout. An error means the call failed,
// not that the check did.
func (e *Env) aws(ctx context.Context, args ...string) (string, error) {
	return e.Run.Capture(ctx, "aws", append(args, "--region", e.Cfg.Cloud.Region, "--output", "text")...)
}

// ─────────────────────────── identity ───────────────────────────

// CheckIdentity asserts the credentials in scope belong to the account the config names.
//
// Provisioning into the wrong account is not a hypothetical: the profile is ambient
// (AWS_PROFILE, SSO session, an assumed role) while the account id is declared in
// rackctl.yaml, and nothing has ever compared them. A mismatch builds a complete,
// healthy platform in someone else's account.
func CheckIdentity(ctx context.Context, env *Env) doctor.Result {
	const name = "aws identity"

	got, err := env.aws(ctx, "sts", "get-caller-identity", "--query", "Account")
	if err != nil {
		return fail(name, "cannot resolve AWS identity — run `aws sso login --profile "+env.Cfg.Cloud.Profile+"`")
	}
	want := env.Cfg.Cloud.AccountID
	if got != want {
		return fail(name, fmt.Sprintf(
			"credentials are for account %s, but rackctl.yaml declares %s — this would provision "+
				"the platform into the wrong account", got, want))
	}
	return ok(name, "account "+got+" matches rackctl.yaml")
}

// ─────────────────────────── quota ───────────────────────────

// CheckQuota asserts there is vCPU headroom for the cluster the config asks for.
//
// A quota shortfall does not fail fast. Karpenter provisions, the fleet request is
// throttled, and pods sit Pending while every rackctl phase reports success.
func CheckQuota(ctx context.Context, env *Env) doctor.Result {
	const name = "vcpu quota"

	out, err := env.aws(ctx, "service-quotas", "get-service-quota",
		"--service-code", "ec2", "--quota-code", "L-1216C47A", "--query", "Quota.Value")
	if err != nil {
		return warn(name, "could not read the EC2 vCPU quota — provisioning may throttle")
	}
	have, err := strconv.ParseFloat(strings.TrimSpace(out), 64)
	if err != nil {
		return warn(name, "unreadable quota value: "+out)
	}
	want := env.Cfg.Quotas.VCPU
	if want > 0 && int(have) < want {
		return fail(name, fmt.Sprintf(
			"standard on-demand vCPU quota is %d, config asks for %d — nodes will sit Pending "+
				"while every phase reports success", int(have), want))
	}
	return ok(name, fmt.Sprintf("%d standard on-demand vCPU available", int(have)))
}

// ─────────────────────────── stale state ───────────────────────────

type tfState struct {
	Resources []json.RawMessage `json:"resources"`
}

// CheckStaleState catches Terraform state that describes a world which no longer exists.
//
// This is the failure that ends a session. Destroy the cluster out of band — a console
// delete, a cost panic, a half-finished rollback — and the state is untouched. It still
// claims the cluster, the VPC, the IAM roles. The next run then reconciles every
// component against resources that are not there, and `terragrunt destroy` cannot clean
// it either: the cluster state's kubernetes and helm providers fail to initialize when
// there is no cluster to initialize against.
//
// The tell is cheap and unambiguous: state holds resources, and the cluster those
// resources belong to does not exist.
func CheckStaleState(ctx context.Context, env *Env) doctor.Result {
	const name = "terraform state"

	cfg := env.Cfg
	envName := string(cfg.Environment)
	bucket := fmt.Sprintf("%s-%s-tfstate", cfg.Cloud.AccountID, cfg.Cloud.Region)

	// Does the cluster the state describes actually exist?
	clusterLive := true
	if _, err := env.aws(ctx, "eks", "describe-cluster", "--name", clusterName(cfg), "--query", "cluster.status"); err != nil {
		clusterLive = false
	}

	var stale []string
	for _, comp := range phases.CoreComponents(cfg) {
		key := fmt.Sprintf("%s/aws/workload-%s/%s/%s/%s/terraform.tfstate",
			envName, envName, cfg.Cloud.Region, envName, comp)

		// s3 cp to stdout: absent state is the normal case on a clean account.
		raw, err := env.Run.Capture(ctx, "aws", "s3", "cp",
			"s3://"+bucket+"/"+key, "-", "--region", cfg.Cloud.Region)
		if err != nil || strings.TrimSpace(raw) == "" {
			continue
		}
		var st tfState
		if json.Unmarshal([]byte(raw), &st) != nil || len(st.Resources) == 0 {
			continue
		}
		stale = append(stale, fmt.Sprintf("%s (%d)", comp, len(st.Resources)))
	}

	switch {
	case len(stale) == 0:
		return ok(name, "no state — a clean provision")
	case clusterLive:
		// State AND cluster exist: this is a re-apply, which is legitimate.
		return ok(name, fmt.Sprintf("%s exists and state tracks it", clusterName(cfg)))
	default:
		// The cluster is gone but components still hold state. Do NOT assume the state is
		// stale and tell the operator to purge it.
		//
		// That inference is wrong and dangerous, and the first real run proved it: a
		// rollback destroyed the cluster and the VPC, but its teardown of secrets and
		// agent-iam failed — so their state was entirely ACCURATE. The IAM operator role,
		// two S3 buckets and a KMS key were all still there. Purging that state on the
		// advice of a preflight would have orphaned every one of them permanently, which
		// is precisely the failure this command exists to prevent.
		//
		// A missing cluster proves the CLUSTER is gone. It proves nothing about what the
		// other components own. So name the safe remedy: destroy first — it is now
		// idempotent and inits before it runs — and purge only what destroy leaves behind
		// with nothing under it.
		return fail(name, fmt.Sprintf(
			"%s is gone, but these components still hold state — %s. This is a partially "+
				"torn-down platform, not necessarily a stale one: their resources may still "+
				"exist and still be billing. Run `rackctl destroy` first; it tears "+
				"down what is actually there. Purge the state objects under s3://%s/%s/ ONLY "+
				"if destroy leaves state behind with no resources under it — purging state "+
				"that still tracks live resources orphans them permanently.",
			clusterName(cfg), strings.Join(stale, ", "), bucket, envName))
	}
}

// ─────────────────────────── collisions ───────────────────────────

// CheckCollisions enumerates resources that survive a cluster's deletion and will make
// the next create fail.
//
// Deleting an EKS cluster and its VPC does not delete the things whose lifecycle is not
// coupled to them. Terraform normally cleans these up; an out-of-band or interrupted
// teardown leaves them, and each one is a hard create-time conflict on the next run.
//
// The KMS alias is the cruel one. `schedule-key-deletion` does NOT free the alias — so a
// half-cleaned account carries an alias pointing at a key that is pending deletion and
// cannot be revived. The next install dies on AliasAlreadyExists, and the only way out
// is to delete the alias by hand.
func CheckCollisions(ctx context.Context, env *Env) doctor.Result {
	const name = "orphan collisions"

	cfg := env.Cfg
	cluster := clusterName(cfg)
	envName := string(cfg.Environment)

	// If the cluster is live this is a re-apply and every "collision" below is simply
	// the platform's own running infrastructure.
	if _, err := env.aws(ctx, "eks", "describe-cluster", "--name", cluster, "--query", "cluster.status"); err == nil {
		return ok(name, cluster+" is live — these resources are its own")
	}

	var found []string
	add := func(what string) { found = append(found, what) }

	// KMS aliases — the create-time conflict that cannot be retried out of.
	//
	// Every alias a component rackctl applies can mint must be listed here, because scheduling
	// the KEY for deletion does not free the ALIAS: the next install dies on
	// AliasAlreadyExists against a key that can no longer be revived. `<cluster>-alerts` is the
	// observability component's, and it became reachable the moment that component went into
	// CoreComponents unconditionally — a check that enumerates two of three aliases passes a
	// run it should have blocked, which is worse than not checking at all.
	for _, alias := range []string{
		"alias/eks/" + cluster,
		"alias/" + envName + "-platform-secrets",
		"alias/" + cluster + "-alerts", // observability's alert-topic CMK
	} {
		if out, err := env.aws(ctx, "kms", "list-aliases", "--query",
			fmt.Sprintf("length(Aliases[?AliasName=='%s'])", alias)); err == nil && strings.TrimSpace(out) != "0" {
			add("KMS " + alias)
		}
	}

	// The EKS audit log group. Terraform owns it, so it is deleted on a real destroy —
	// but it survives a console delete, and then the cluster apply hits
	// ResourceAlreadyExistsException.
	if out, err := env.aws(ctx, "logs", "describe-log-groups",
		"--log-group-name-prefix", "/aws/eks/"+cluster+"/cluster",
		"--query", "length(logGroups)"); err == nil && strings.TrimSpace(out) != "0" {
		add("log group /aws/eks/" + cluster + "/cluster")
	}

	// Karpenter's interruption queue.
	if out, err := env.aws(ctx, "sqs", "list-queues",
		"--queue-name-prefix", "Karpenter-"+cluster, "--query", "length(QueueUrls)"); err == nil &&
		strings.TrimSpace(out) != "0" && strings.TrimSpace(out) != "" {
		add("SQS Karpenter-" + cluster)
	}

	// IAM roles the cluster and the operator mint. The operator's tenant baseline policy
	// attaches to two roles, so a stale one also blocks agent-iam on DeleteConflict.
	if out, err := env.Run.Capture(ctx, "aws", "iam", "list-roles", "--query",
		fmt.Sprintf("length(Roles[?starts_with(RoleName,'%s-')])", envName), "--output", "text"); err == nil {
		if n, _ := strconv.Atoi(strings.TrimSpace(out)); n > 0 {
			add(fmt.Sprintf("%d IAM role(s) named %s-*", n, envName))
		}
	}

	// The cluster's OIDC provider outlives it.
	if out, err := env.Run.Capture(ctx, "aws", "iam", "list-open-id-connect-providers",
		"--query", fmt.Sprintf("length(OpenIDConnectProviderList[?contains(Arn,'oidc.eks.%s')])", cfg.Cloud.Region),
		"--output", "text"); err == nil && strings.TrimSpace(out) != "0" {
		add("EKS OIDC provider")
	}

	if len(found) == 0 {
		return ok(name, "no orphans from a previous run")
	}
	return fail(name, fmt.Sprintf(
		"a previous cluster left resources that will collide on create — %s. Note that "+
			"scheduling a KMS key for deletion does NOT free its alias: the alias must be "+
			"deleted explicitly or the next install fails on AliasAlreadyExists against a key "+
			"that can no longer be revived.", strings.Join(found, ", ")))
}

// ─────────────────────────── soft-deleted secrets ───────────────────────────

// CheckSoftDeletedSecrets catches secrets sitting in Secrets Manager's recovery window.
//
// A deleted secret is not gone — by default it lingers for 7 to 30 days, and its NAME
// stays taken. Terraform then cannot create a secret of the same name, and the error
// says nothing about a recovery window. The platform's own secrets set
// recovery_window_in_days = 0 precisely so that a teardown/recreate cycle does not trip
// on this; a secret created any other way still will.
func CheckSoftDeletedSecrets(ctx context.Context, env *Env) doctor.Result {
	const name = "soft-deleted secrets"

	// The names the platform will create. managed-monitoring names both secrets after the
	// full cluster name (<environment>-<base>-grafana-token,
	// <environment>-<base>-managed-monitoring-endpoints), so derive them from the config
	// rather than pinning literals — a hardcoded name silently stops matching the moment the
	// cluster is named anything but the default, and the check would then pass vacuously
	// against a substrate whose secrets it never looked at. A soft-deleted secret under
	// either name blocks the component that owns it.
	cluster := clusterName(env.Cfg)
	want := []string{cluster + "-grafana-token", cluster + "-managed-monitoring-endpoints"}

	out, err := env.Run.Capture(ctx, "aws", "secretsmanager", "list-secrets",
		"--include-planned-deletion",
		"--region", env.Cfg.Cloud.Region,
		"--query", "SecretList[?DeletedDate!=null].Name", "--output", "text")
	if err != nil {
		return warn(name, "could not list secrets")
	}

	pending := strings.Fields(out)
	var blocked []string
	for _, p := range pending {
		for _, w := range want {
			if p == w {
				blocked = append(blocked, p)
			}
		}
	}
	if len(blocked) == 0 {
		return ok(name, "no name is held by a pending deletion")
	}
	return fail(name, fmt.Sprintf(
		"%s are scheduled for deletion but not yet gone — the NAME stays taken for the whole "+
			"recovery window, so terraform cannot recreate them. Force-delete them: "+
			"aws secretsmanager delete-secret --force-delete-without-recovery --secret-id <name>",
		strings.Join(blocked, ", ")))
}

// ─────────────────────────── catalog fork ───────────────────────────

// CheckCatalogFork asserts the org's catalog fork is current with upstream.
//
// The cluster reads its addon catalog from the FORK, never from upstream. A fork left at
// whatever commit it was forked at means a fix merged upstream this morning is simply
// absent from the cluster built this afternoon — and the run meant to prove that fix
// quietly disproves it. Nothing errors; the catalog is valid, just old.
//
// A fork that is AHEAD of upstream is not a problem. The org owns it and is expected to
// commit to it — that is the whole reason the catalog is forked rather than consumed.
func CheckCatalogFork(ctx context.Context, env *Env) doctor.Result {
	const name = "catalog fork"

	// An org that publishes the catalog has nothing to fall behind. Without this the check
	// compares main against itself, always reads zero, and reports "nanohype/eks-gitops is
	// current with nanohype/eks-gitops" — a green tick for a comparison that cannot fail,
	// which is indistinguishable from a check that is working.
	if engine.OwnsUpstreamCatalog(env.Cfg.Org.Name) {
		return ok(name, env.Cfg.Org.Name+" publishes "+engine.UpstreamCatalog+
			" — no fork exists to fall behind it")
	}

	fork := env.Cfg.Org.Name + "/eks-gitops"
	upstream := engine.UpstreamCatalog

	if _, err := env.Run.Capture(ctx, "gh", "repo", "view", fork, "--json", "name"); err != nil {
		return ok(name, fork+" does not exist yet — init will fork it")
	}

	out, err := env.Run.Capture(ctx, "gh", "api",
		fmt.Sprintf("repos/%s/compare/%s:main...%s:main", fork, env.Cfg.Org.Name, engine.UpstreamCatalogOwner()),
		"--jq", ".ahead_by")
	if err != nil {
		return warn(name, "could not compare "+fork+" with "+upstream)
	}
	// Do NOT swallow a parse error into a zero. `behind, _ := Atoi(...)` reads an
	// unparseable response as "0 commits behind" — i.e. as HEALTHY — which is precisely
	// the green-light-that-means-nothing this command exists to eliminate. If we cannot
	// tell, we say so.
	behind, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return warn(name, fmt.Sprintf("could not read how far %s is behind %s (got %q) — "+
			"verify by hand before relying on this run", fork, upstream, truncate(out, 60)))
	}
	if behind > 0 {
		return fail(name, fmt.Sprintf(
			"%s is %d commit(s) behind %s — the cluster reads its catalog from the FORK, so it "+
				"would be built without those changes and nothing would report it. "+
				"Fix: gh repo sync %s --source %s --branch main",
			fork, behind, upstream, fork, upstream))
	}
	return ok(name, fork+" is current with "+upstream)
}

// ─────────────────────────── github credential ───────────────────────────

// CheckGitHubToken asserts a GitHub credential exists when the config arms the one
// component that needs one.
//
// org.gitops.tenantsRepo is the trigger. It makes rackctl send TF_VAR_tenants_repo_url,
// and cluster-bootstrap's own comment states the contract exactly: "the token comes from
// the GITHUB_TOKEN environment variable. When tenants_repo_url is empty, owner is "" and
// no github resources are created, so the provider is never called"
// (components/aws/cluster-bootstrap/main.tf:170-172). Setting the repo is what calls it.
//
// Unauthenticated, that provider 401s during PHASE 5 — after the VPC, the EKS cluster and
// every substrate component are built and billing. Cheap to know now, expensive to learn
// then.
//
// The trap this check really exists for: the documented setup path PRODUCES the failing
// state. quickstart.md tells operators to run `gh auth login`, which stores the credential
// in gh's keyring and exports nothing at all. Following the docs exactly left GITHUB_TOKEN
// unset, so the install that was set up correctly is the install that fails.
func CheckGitHubToken(ctx context.Context, env *Env) doctor.Result {
	const name = "github token"

	repo := env.Cfg.Org.GitOps.TenantsRepo
	if repo == "" {
		return ok(name, "org.gitops.tenantsRepo is unset — cluster-bootstrap's github provider is never called")
	}
	if os.Getenv("GITHUB_TOKEN") != "" {
		return ok(name, "GITHUB_TOKEN is exported")
	}
	// Require a non-empty token, not merely a zero exit. `gh auth token` prints nothing and
	// still succeeds in some states, and "it exited 0" is the green light that means nothing.
	if tok, err := env.Run.Capture(ctx, "gh", "auth", "token"); err == nil && tok != "" {
		return ok(name, "gh holds a token — rackctl passes it to cluster-bootstrap as GITHUB_TOKEN")
	}
	return fail(name, fmt.Sprintf(
		"org.gitops.tenantsRepo is set (%s) but no GitHub credential is reachable. That setting "+
			"arms cluster-bootstrap's `provider \"github\"`, which authenticates from GITHUB_TOKEN "+
			"and nothing else — without it the apply 401s in phase 5, after the VPC and the EKS "+
			"cluster are already built. Fix: `gh auth login` (rackctl bridges the token for you), "+
			"or export GITHUB_TOKEN with repo scope on %s. Or unset org.gitops.tenantsRepo to skip "+
			"the tenant-repo deploy-key wiring entirely.", repo, repo))
}

// ─────────────────────────── local vend ───────────────────────────

// CheckVendFreshness asserts the local checkouts are current with their remotes.
//
// Same bug as the fork, one layer down: "present" is not "current". A stale landing-zone
// checkout provisions with yesterday's terraform and reports success.
func CheckVendFreshness(ctx context.Context, env *Env) doctor.Result {
	const name = "local vend"

	repos := engine.RepoPaths(env.Cfg.Org.Name)
	dirs := map[string]string{
		"landing-zone":       repos.LandingZone,
		"eks-gitops":         repos.EKSGitops,
		"eks-agent-platform": repos.AgentPlatform,
	}

	var behind []string
	prev := env.Run.Dir
	defer func() { env.Run.Dir = prev }()

	for repo, dir := range dirs {
		env.Run.Dir = dir
		if _, err := env.Run.Capture(ctx, "git", "rev-parse", "--git-dir"); err != nil {
			continue // not cloned yet — init will clone it
		}
		if _, err := env.Run.Capture(ctx, "git", "fetch", "--quiet", "origin"); err != nil {
			continue // offline; not preflight's business to fail on
		}
		out, err := env.Run.Capture(ctx, "git", "rev-list", "--count", "HEAD..@{upstream}")
		if err != nil {
			continue
		}
		if n, _ := strconv.Atoi(strings.TrimSpace(out)); n > 0 {
			behind = append(behind, fmt.Sprintf("%s (%d)", repo, n))
		}
	}

	if len(behind) == 0 {
		return ok(name, "checkouts are current")
	}
	return warn(name, fmt.Sprintf(
		"%s behind their remote — init fast-forwards them, but a divergence would be used as-is",
		strings.Join(behind, ", ")))
}

// ─────────────────────────── cost pipeline prerequisite ───────────────────────────

// curExportParams is the contract cost-pipeline resolves the Cost and Usage Report from.
//
// Read through unguarded `data "aws_ssm_parameter"` blocks
// (eks-agent-platform/terraform/components/cost-pipeline/main.tf:53-63), so a missing
// parameter is not a degraded feature — it is a plan-time failure of the whole root.
var curExportParams = []string{
	"/platform/org/cost/cur-export-bucket",
	"/platform/org/cost/cur-export-prefix",
	"/platform/org/cost/cur-export-name",
}

// costOrgRoot is the only thing in the org that writes them: landing-zone's org-cost.
const costOrgRoot = "live/aws/management/<region>/org/org-cost"

// CheckCostPipelinePrerequisite asserts the CUR contract exists before the install starts.
//
// A Cost and Usage Report has no filter — it always covers the whole account — so exactly
// one exists per account, and defining it is an org-level act in the management account.
// rackctl deliberately does not perform it: applying landing-zone's org-cost would also
// enroll the account in Compute Optimizer, create org budgets and stand up anomaly
// monitors, none of which a platform install should do on the way past.
//
// So the prerequisite is real and rackctl's job is to say so early. Without it the apply
// dies at cost-pipeline, which is root TWO of the seven the agent-platform substrate
// applies — and that phase runs after the VPC, the EKS control plane, the whole AWS
// substrate and ArgoCD convergence. The operator pays for a cluster and a NAT gateway to
// discover a missing SSM parameter.
//
// The two failure modes this must distinguish, because they want opposite remedies:
// parameters absent (apply org-cost, or turn the tier off) versus the query itself failing
// (unknown — warn, never a green tick).
func CheckCostPipelinePrerequisite(ctx context.Context, env *Env) doctor.Result {
	const name = "cost pipeline"

	if !env.Cfg.AgentPlatform.Enabled() {
		return ok(name, "agent platform is off — no cost roots to apply")
	}
	if !env.Cfg.AgentPlatform.CostPipelineEnabled() {
		return ok(name, "agentPlatform.costPipeline is false — cost-pipeline and cost-access are "+
			"not applied, so BudgetPolicy has no per-tenant spend to measure")
	}

	// get-parameters returns the names it could NOT resolve in InvalidParameters, so one call
	// answers for all three and names exactly which are missing. Querying that field rather
	// than counting Parameters keeps the failure message specific: "which one" is the first
	// thing the operator asks.
	args := append([]string{"ssm", "get-parameters", "--names"}, curExportParams...)
	out, err := env.aws(ctx, append(args, "--query", "InvalidParameters")...)
	if err != nil {
		return warn(name, fmt.Sprintf(
			"could not read the CUR export parameters (%s) — verify by hand before relying on "+
				"this run; cost-pipeline resolves them through unguarded data blocks and fails "+
				"the plan if they are absent", strings.Join(curExportParams, ", ")))
	}

	// `--output text` renders an empty list as an empty string and a populated one as
	// tab-separated names. Fields() handles both, and the "None" AWS prints for a null.
	missing := strings.Fields(out)
	if len(missing) == 1 && missing[0] == "None" {
		missing = nil
	}
	if len(missing) == 0 {
		return ok(name, "the CUR export contract is published — cost-pipeline and cost-access can resolve it")
	}

	return fail(name, fmt.Sprintf(
		"%s missing in this account. cost-pipeline reads them through unguarded data blocks, so it "+
			"fails at PLAN — as root two of the seven the agent-platform substrate applies, i.e. "+
			"after the VPC, the cluster and ArgoCD are built and billing. Only landing-zone's "+
			"org-cost root publishes them, and it belongs to the management account because a CUR "+
			"is an account singleton. Fix: apply %s, or set agentPlatform.costPipeline: false to "+
			"install without the cost tier",
		strings.Join(missing, ", "), costOrgRoot))
}
