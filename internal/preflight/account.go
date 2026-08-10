package preflight

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/doctor"
)

// The checks in this file are about the ACCOUNT rather than about a previous run of this
// platform, and that distinction is the reason they exist.
//
// The rest of preflight assumes the account is the platform's own and asks whether the last
// attempt left wreckage in it. That assumption is not safe. rackctl's shipped topology is one
// account per startup — rung one of the maturity ladder — and one account per startup means the
// account fills up with everything else the startup does: other environments, other projects,
// a marketing site, a mail domain, whatever the business needed before it needed a platform.
//
// So the questions here are not "did we leave a mess" but "is the thing we are about to claim
// already somebody's". Every one of them is knowable in seconds and none of them was checked.

// ─────────────────────────── credential lifetime ───────────────────────────

// CheckSessionLifetime asserts the credentials will outlive the install.
//
// CheckIdentity proves the credentials work NOW. That is a different question, and the gap
// between the two is a whole provision: a run that starts with four minutes of SSO session left
// authenticates fine, builds a VPC, and then dies partway through the cluster phase with a
// token that cannot be refreshed. Clean-on-failure then attempts a rollback with no credentials
// to do it, so the run leaves a half-built VPC and NAT gateway behind and cannot tell you.
//
// A run that dies half-applied because a token lapsed mid-phase is strictly worse than one that
// refuses at the start, and which of the two you get is decided before anything is spent.
func CheckSessionLifetime(ctx context.Context, env *Env) doctor.Result {
	const name = "session lifetime"

	// A full install is dominated by the EKS control plane (~10 min) and the addon
	// convergence wait; an hour is the honest floor for the whole pipeline, and the number is
	// stated rather than tuned because a threshold nobody can explain gets raised the first
	// time it is inconvenient.
	const needed = time.Hour

	raw, err := env.Run.Capture(ctx, "aws", "configure", "export-credentials",
		"--profile", env.Cfg.Cloud.Profile, "--format", "process")
	if err != nil {
		// Static credentials, an instance role, or an env-var pair have no expiry to read and
		// no expiry to worry about. Not being able to answer is not the same as a short session.
		return ok(name, "no expiring session credentials to check")
	}
	var creds struct {
		Expiration string `json:"Expiration"`
	}
	if json.Unmarshal([]byte(raw), &creds) != nil || creds.Expiration == "" {
		return ok(name, "credentials carry no expiry")
	}
	exp, err := time.Parse(time.RFC3339, creds.Expiration)
	if err != nil {
		return warn(name, "could not read the credential expiry ("+truncate(creds.Expiration, 40)+")")
	}

	left := time.Until(exp).Round(time.Minute)
	switch {
	case left <= 0:
		return fail(name, "the session has already expired — run `aws sso login --profile "+
			env.Cfg.Cloud.Profile+"`")
	case left < needed:
		return fail(name, fmt.Sprintf(
			"only %s of session left, and a full install needs about an hour. A token that lapses "+
				"mid-phase leaves a half-applied run that cannot even roll itself back, because the "+
				"rollback needs the same credentials. Refresh first: aws sso login --profile %s",
			left, env.Cfg.Cloud.Profile))
	}
	return ok(name, fmt.Sprintf("%s of session left", left))
}

// ─────────────────────────── bucket names ───────────────────────────

// bucketScope says how many of a bucket there is meant to be, which is what decides whether
// finding one already there is wreckage or the steady state.
//
// Getting this wrong in either direction costs a run. Treating a shared bucket as cluster-scoped
// made this check fail on the account it was written against, and would have failed on every
// account that had ever run an install — a preflight that fires on the healthy steady state is
// worse than no preflight, because it teaches the operator to skip it. Treating a cluster bucket
// as shared loses the check that motivated this file.
type bucketScope int

const (
	// scopeCluster: one per cluster. An existing one is this cluster's own wreckage — a previous
	// run, or a teardown that could not empty it — and it blocks the install.
	scopeCluster bucketScope = iota
	// scopeAccount: one per account+region, shared by every environment. eks-agent-platform's
	// live/org roots own these; their names carry no cluster and no environment token because
	// there is exactly one of the thing they name. Whichever environment installs first creates
	// them, so from the second environment onwards finding them is expected.
	scopeAccount
	// scopeBackend: Terraform state. Created idempotently behind a head-bucket guard
	// (landing-zone scripts/init-backend-aws.sh:12, phases/agentplatform.go:224), never deleted
	// by rackctl, and shared across every environment in the account, differing only by key.
	scopeBackend
)

// plannedBucket is a bucket this config will try to create, and the component that owns it.
//
// Every scope still gets the other two checks. A name over 63 characters is fatal regardless,
// and a 403 — the name existing in somebody else's account — is MORE fatal on a backend than
// anywhere else, because without one nothing can be applied at all.
type plannedBucket struct {
	name, owner string
	scope       bucketScope
}

// plannedBuckets returns every S3 bucket name this config would claim.
//
// Composed from the components' own expressions rather than guessed:
//
//	agent-iam        artifacts.tf:50-52  <cluster>-<account>-<region>-{model-artifacts,eval-reports,access-logs}
//	cluster-addons   main.tf:37 + s3.tf  <cluster>-<account>-<region>-{velero,loki,tempo,argo-workflows}
//	model-import     main.tf:65          <environment>-<account>-<region>-model-import
//	bedrock-account  main.tf:11,92,152   org-<account>-<region>-bedrock-{access-logs,invocations}
//	cost-pipeline    main.tf:67,169,237,335
//	                                     org-<account>-<region>-cost-{access-logs,estimates,athena}-<account>
//	backends         init-backend-aws.sh:9 / agentplatform.go:214
//
// The last two used to be composed as <cluster>-bedrock-* and <cluster>-cost-*, which is the
// shape eks-agent-platform had before it account-scoped both components. Those names are not
// stale in the harmless sense — nothing creates them now, so the check was looking for names
// that cannot exist while missing the ones that do, which reads as a clean preflight over an
// unchecked estate.
//
// The account id appears twice in the cost names and once in the bedrock ones. That asymmetry
// is upstream's, not a typo here: cost-pipeline suffixes each bucket with the caller's account
// on top of a prefix that already carries it (main.tf:169), and bedrock-account does not.
//
// Note what left with the rename: these five no longer contain cluster.name at all, so the
// 63-character pressure that made cluster.name the fix for a too-long name is now confined to
// the cluster-scoped set.
//
// Not listed: org-<account>-cur-export. It is landing-zone's, created by its org-cost root,
// and rackctl does not apply that root — so it is not a name this config claims.
//
// cluster-addons' four are listed unconditionally even though a leaf can disable a consumer
// (staging turns argo-workflows off, so that bucket is never created). Over-inclusion costs one
// HeadBucket call and a name that is already taken is worth knowing about either way;
// under-inclusion costs a provision.
func plannedBuckets(cfg *config.Config) []plannedBucket {
	cluster := cfg.ClusterName()
	acct := cfg.Cloud.AccountID
	region := cfg.Cloud.Region
	envName := string(cfg.Environment)

	bs := []plannedBucket{
		{name: fmt.Sprintf("%s-%s-tfstate", acct, region), owner: "terraform backend", scope: scopeBackend},
	}
	for _, s := range []string{"velero", "loki", "tempo", "argo-workflows"} {
		bs = append(bs, plannedBucket{name: fmt.Sprintf("%s-%s-%s-%s", cluster, acct, region, s), owner: "cluster-addons"})
	}
	if cfg.AgentPlatform.Enabled() {
		for _, s := range []string{"model-artifacts", "eval-reports", "access-logs"} {
			bs = append(bs, plannedBucket{name: fmt.Sprintf("%s-%s-%s-%s", cluster, acct, region, s), owner: "agent-iam"})
		}
		bs = append(bs, plannedBucket{
			name:  fmt.Sprintf("eks-agent-platform-tfstate-%s-%s", acct, region),
			owner: "agent-platform backend", scope: scopeBackend})
		for _, s := range []string{"access-logs", "invocations"} {
			bs = append(bs, plannedBucket{
				name:  fmt.Sprintf("%s-%s-%s-bedrock-%s", accountScopeToken, acct, region, s),
				owner: "bedrock-account", scope: scopeAccount})
		}
		for _, s := range []string{"access-logs", "estimates", "athena"} {
			bs = append(bs, plannedBucket{
				name:  fmt.Sprintf("%s-%s-%s-cost-%s-%s", accountScopeToken, acct, region, s, acct),
				owner: "cost-pipeline", scope: scopeAccount})
		}
		if cfg.AgentPlatform.ModelImport {
			bs = append(bs, plannedBucket{
				name: fmt.Sprintf("%s-%s-%s-model-import", envName, acct, region), owner: "model-import"})
		}
	}
	return bs
}

// accountScopeToken is the environment token eks-agent-platform's account-scoped roots carry.
//
// It is a literal rather than anything derived from cfg.Environment, and that is the point:
// terraform/live/org/env.hcl pins `environment = "org"` for every account-scoped root, so these
// names are the same whichever environment rackctl is installing. Composing them from the
// config would produce development-…-bedrock-invocations, which nothing creates.
//
// `org` is the reserved account-scope token from nanohype/standards/resource-naming.json, the
// same one landing-zone uses for its management-account roots.
const accountScopeToken = "org"

// CheckBucketNames asserts every bucket this run would create can actually be created.
//
// This package's own header opens with the failure it was written for — "BucketAlreadyExists on
// a bucket name that is globally unique across every AWS account on earth. Unrecoverable by
// retry. Discovered 6 minutes in." — and until now nothing here checked a single bucket name.
// The motivating example was the one gap.
//
// Three outcomes, and conflating them would waste the check:
//
//   - 404: free.
//   - 200: the bucket exists AND this account owns it. Wreckage from a previous run, or a
//     component whose teardown could not empty it. Recoverable — destroy, or empty it.
//   - 403: the bucket exists and SOMEBODY ELSE owns it. The S3 namespace is global, so this
//     name is gone and no amount of cleanup in this account frees it. The only fix is to change
//     cluster.name. This is the unrecoverable one and it must not read like the other two.
//
// Length is checked in the same pass. cluster-addons has an upstream precondition for its own
// four (s3.tf:316) with one character of headroom at the worst case; agent-iam's and the
// agent-platform tree's have none at all, and a 64-character name fails at create with an error
// about naming rules rather than about cluster.name.
func CheckBucketNames(ctx context.Context, env *Env) doctor.Result {
	const name = "bucket names"

	cfg := env.Cfg
	planned := plannedBuckets(cfg)

	// Every bucket this account already owns, in one call. list-buckets is global and returns
	// buckets in every region regardless of the --region flag, which is exactly the property
	// that makes a name-pattern sweep cross regions whether it means to or not.
	owned := map[string]bool{}
	if raw, err := env.Run.Capture(ctx, "aws", "s3api", "list-buckets",
		"--query", "Buckets[].Name", "--output", "text"); err == nil {
		for _, b := range strings.Fields(raw) {
			owned[b] = true
		}
	}

	// A live cluster means this is a re-apply and its own buckets are not collisions.
	reapply := false
	if _, err := env.aws(ctx, "eks", "describe-cluster", "--name", clusterName(cfg), "--query", "cluster.status"); err == nil {
		reapply = true
	}

	var tooLong, taken, ours, shared []string
	for _, b := range planned {
		if len(b.name) > 63 {
			tooLong = append(tooLong, fmt.Sprintf("%s (%d chars, %s)", b.name, len(b.name), b.owner))
			continue
		}
		if owned[b.name] {
			switch {
			case reapply:
				// A re-apply owns its own buckets.
			case b.scope == scopeBackend:
				// A state backend that already exists is the steady state, not a collision.
			case b.scope == scopeAccount:
				// One per account+region, shared by every environment. Another environment
				// having created it is the ordinary case from the second install onwards, so
				// this is reported and not refused — see the warn branch below.
				shared = append(shared, fmt.Sprintf("%s (%s)", b.name, b.owner))
			case b.scope == scopeCluster:
				ours = append(ours, fmt.Sprintf("%s (%s)", b.name, b.owner))
			}
			continue
		}
		// Not ours. Ask S3 whether it exists at all — a 403 means it does and belongs to
		// another account, which no cleanup here can undo.
		if _, err := env.Run.Capture(ctx, "aws", "s3api", "head-bucket", "--bucket", b.name); err == nil {
			// Reachable and not in list-buckets: an access grant rather than ownership.
			taken = append(taken, fmt.Sprintf("%s (%s)", b.name, b.owner))
		}
	}

	switch {
	case len(tooLong) > 0:
		return fail(name, fmt.Sprintf(
			"these bucket names exceed S3's 63-character limit — %s. The length comes from "+
				"cluster.name plus the account id and region, so shortening cluster.name is the fix; "+
				"the apply would otherwise fail with a message about S3 naming rules that never "+
				"mentions the knob.", strings.Join(tooLong, ", ")))
	case len(taken) > 0:
		return fail(name, fmt.Sprintf(
			"these bucket names already exist and are NOT owned by this account — %s. S3's namespace "+
				"is global, so these names are gone and nothing you delete here will free them. The "+
				"only fix is a different cluster.name. (Discovering this at apply time costs a VPC and "+
				"an EKS control plane first.)", strings.Join(taken, ", ")))
	case len(ours) > 0:
		return fail(name, fmt.Sprintf(
			"these buckets already exist in this account and %s is not live — %s. They are wreckage "+
				"from a previous run, or buckets a teardown could not empty. Run `rackctl destroy "+
				"--apply` (add --force-buckets outside development), or empty and delete them by "+
				"hand. Note that a bucket left behind means its component's terraform state and the "+
				"account have diverged.", clusterName(cfg), strings.Join(ours, ", ")))
	case len(shared) > 0:
		// A warning rather than a failure, and the asymmetry is deliberate. These are
		// account+region singletons owned by eks-agent-platform's live/org roots, so on the
		// second and every later environment their existence is exactly what a healthy account
		// looks like — refusing would make this check fire on the steady state, which is the
		// failure mode the scope split exists to avoid.
		//
		// It is still worth saying, because on a FIRST install of a FIRST environment the same
		// observation means wreckage, and rackctl cannot tell those two apart without knowing
		// whether another environment exists. Naming them lets the operator make that call.
		return warn(name, fmt.Sprintf(
			"these account-scoped buckets already exist — %s. They are one per account and region, "+
				"shared by every environment, so if another environment is already installed here "+
				"this is the expected steady state and the apply will adopt them through their own "+
				"state. If this is the first install in this account, they are wreckage from an "+
				"earlier attempt and the org roots will fail at create.", strings.Join(shared, ", ")))
	}
	return ok(name, fmt.Sprintf("all %d bucket names are free", len(planned)))
}

// ─────────────────────────── route53 ───────────────────────────

// CheckHostedZone asserts the run is not about to mint a second zone for a domain that already
// has one in this account.
//
// dns runs in CREATE mode: the apply mints a hosted zone and the destroy deletes one. Two public
// zones for the same name is not an error to AWS — it is a supported thing to do — and the
// registrar's NS delegation points at exactly one of them. So the failure is silent in both
// directions: external-dns writes records into a zone nothing resolves, and a later teardown
// deletes a zone that may be the one that WAS being resolved.
//
// This is not hypothetical in the account this was written for. It holds 12 public zones, three
// of them carrying live MX records, and every plausible value of dns.hostedZone for this org is
// already one of them.
func CheckHostedZone(ctx context.Context, env *Env) doctor.Result {
	const name = "hosted zone"

	cfg := env.Cfg
	if cfg.DNS == nil || cfg.DNS.HostedZone == "" {
		return ok(name, "no dns block — the dns component is not applied")
	}
	want := strings.TrimSuffix(cfg.DNS.HostedZone, ".") + "."

	// Route53 is global; the --region the other checks carry is meaningless here and is left off
	// deliberately rather than by accident.
	raw, err := env.Run.Capture(ctx, "aws", "route53", "list-hosted-zones",
		"--query", "HostedZones[].[Name,Id,Config.PrivateZone]", "--output", "text")
	if err != nil {
		return warn(name, "could not list hosted zones")
	}

	// The zones this run's own dns state already tracks. A zone terraform owns is not a
	// collision with anything — re-applying it is a no-op, and the alternative reading makes
	// `rackctl apply` refuse to run a second time against a platform it built itself.
	//
	// That is not a hypothetical cost. Resuming an install after any later phase fails is
	// exactly when this check runs against a zone the earlier phases created, and refusing
	// there leaves the operator with a provisioned cluster and no supported way forward.
	// CheckStaleState already draws this distinction for every other component ("state exists
	// and the cluster exists" is a re-apply, not a fault); this is the same rule for the one
	// resource whose duplicate is invisible to AWS.
	owned := ownedZoneIDs(ctx, env)

	var existing, foreign []string
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == want {
			kind := "public"
			if len(f) >= 3 && strings.EqualFold(f[2], "true") {
				kind = "private"
			}
			entry := fmt.Sprintf("%s (%s)", f[1], kind)
			existing = append(existing, entry)
			// Route53 reports the id as /hostedzone/ZXXXX; state records the bare id.
			if !owned[strings.TrimPrefix(f[1], "/hostedzone/")] {
				foreign = append(foreign, entry)
			}
		}
	}
	switch {
	case len(existing) == 0:
		return ok(name, want+" has no zone in this account — dns will mint it")
	case len(foreign) == 0:
		return ok(name, fmt.Sprintf(
			"%s is the zone this install's dns state already tracks (%s) — a re-apply reconciles it "+
				"rather than minting a second one", want, strings.Join(existing, ", ")))
	}
	return fail(name, fmt.Sprintf(
		"%s already has %d hosted zone(s) in this account that this install does not own — %s. The "+
			"dns component runs in create mode, so it would mint ANOTHER one; AWS allows that, and "+
			"the registrar's NS delegation points at only one. external-dns would then publish into "+
			"a zone nothing resolves, and a later `rackctl destroy` would delete a zone that may be "+
			"the live one. If this domain is already delegated, dns.hostedZone is not the right way "+
			"to reach it.",
		want, len(foreign), strings.Join(foreign, ", ")))
}

// ownedZoneIDs returns the Route53 zone ids recorded in this install's dns state.
//
// Read from the state object in S3 rather than through terragrunt: this runs before any
// component is initialised, and `terragrunt state list` needs a backend reinit it has no
// business performing during a read-only preflight.
//
// An unreadable or absent state yields an empty set, which is the conservative answer — the
// check then treats every existing zone as foreign and fails, which is the behaviour that
// was there before. Being unable to prove ownership must never read as ownership.
func ownedZoneIDs(ctx context.Context, env *Env) map[string]bool {
	cfg := env.Cfg
	envName := string(cfg.Environment)
	key := fmt.Sprintf("%s/aws/workload-%s/%s/%s/dns/terraform.tfstate",
		envName, envName, cfg.Cloud.Region, envName)
	bucket := fmt.Sprintf("%s-%s-tfstate", cfg.Cloud.AccountID, cfg.Cloud.Region)

	raw, err := env.Run.Capture(ctx, "aws", "s3", "cp",
		"s3://"+bucket+"/"+key, "-", "--region", cfg.Cloud.Region)
	if err != nil || strings.TrimSpace(raw) == "" {
		return nil
	}
	var st struct {
		Resources []struct {
			Type      string `json:"type"`
			Instances []struct {
				Attributes struct {
					ZoneID string `json:"zone_id"`
				} `json:"attributes"`
			} `json:"instances"`
		} `json:"resources"`
	}
	if json.Unmarshal([]byte(raw), &st) != nil {
		return nil
	}
	out := map[string]bool{}
	for _, r := range st.Resources {
		if r.Type != "aws_route53_zone" {
			continue
		}
		for _, i := range r.Instances {
			if id := i.Attributes.ZoneID; id != "" {
				out[id] = true
			}
		}
	}
	return out
}

// ─────────────────────────── bedrock's account singleton ───────────────────────────

// CheckBedrockLogging asserts this run will not silently take over somebody else's Bedrock
// invocation logging.
//
// aws_bedrock_model_invocation_logging_configuration (eks-agent-platform
// components/bedrock-account/main.tf) has no name and no identifier: the Bedrock API holds
// EXACTLY ONE configuration per account per region.
//
// It used to be applied per environment, which made this check's warning the whole story —
// applying development overwrote production's logging destination and tearing development down
// deleted the singleton outright, both applies green, with invocation logging being the signal
// every budget decision reads. That was ledger O14, and upstream fixed the shape rather than the
// symptom: the configuration and the two buckets it points at moved to an account-scoped root,
// terraform/live/org/bedrock-account, whose names carry no cluster and no environment token
// because there is exactly one of the thing they name.
//
// So what remains is narrower and still worth having. The singleton is genuinely account-global
// in an account this platform SHARES with other estates, so a configuration pointing anywhere
// other than the account root's own bucket belongs to something else — and this run would
// repoint it without saying so.
//
// The name below is composed from cfg.Cloud.Region, while the org roots resolve theirs from
// terraform/live/org/env.hcl, which pins us-west-2. The two agree today and would diverge for a
// config in another region — but so would the deployment, since that root would apply its
// logging to us-west-2 while the cluster ran elsewhere. Composing from the config is the shape
// that stays right when the pin is lifted; the pin itself is upstream's to lift.
func CheckBedrockLogging(ctx context.Context, env *Env) doctor.Result {
	const name = "bedrock logging"

	cfg := env.Cfg
	if !cfg.AgentPlatform.Enabled() {
		return ok(name, "agent platform disabled — the bedrock component is not applied")
	}

	bucket, err := env.aws(ctx, "bedrock", "get-model-invocation-logging-configuration",
		"--query", "loggingConfig.s3Config.bucketName")
	if err != nil || bucket == "" || bucket == "None" {
		return ok(name, "no invocation logging configured in "+cfg.Cloud.Region)
	}

	// The bucket the account-scoped root delivers to. If it matches, the singleton is already
	// this platform's and re-applying is a no-op rather than a takeover — including from an
	// environment that did not create it, which is the ordinary case for every environment after
	// the first.
	mine := fmt.Sprintf("%s-%s-%s-bedrock-invocations", accountScopeToken, cfg.Cloud.AccountID, cfg.Cloud.Region)
	if bucket == mine {
		return ok(name, "the invocation logging singleton is this platform's account-scoped one")
	}
	return fail(name, fmt.Sprintf(
		"Bedrock invocation logging in %s is already configured, delivering to %q — and this run's "+
			"bedrock-account root would repoint it at %q. There is exactly ONE such configuration per "+
			"account per region: it has no name and nothing to scope it by, so applying here takes it "+
			"over silently and a later teardown DELETES it rather than restoring the previous owner. "+
			"Invocation logging is what every budget decision reads, so whatever loses it stops being "+
			"able to bill or cap anything, with nothing going red. This account is shared with other "+
			"estates, so the likeliest owner of %q is one of them rather than a previous run of this "+
			"platform. Set agentPlatform.enable: false, or use an account that does not already have "+
			"this set.",
		cfg.Cloud.Region, bucket, mine, bucket))
}

// ─────────────────────────── cost allocation tags ───────────────────────────

// CheckCostAllocationTags warns when the tags the budget pipeline reads are not activated.
//
// This is a WARNING rather than a failure, and the reason is the interesting part: it does not
// block an install, it silently truncates one. Cost allocation tag activation is payer-level,
// global, and NOT RETROACTIVE — a tag activated after a tenant exists produces NULL for every
// hour before activation, permanently. So the cost of getting it wrong is not paid at apply
// time, it is paid a month later when the first budget report covers a partial month and nobody
// can say why.
//
// It belongs in preflight rather than in the probe phase for exactly that reason: by the time a
// probe could observe it, the data it would have needed is already unrecoverable.
//
// # Two halves of one bill, and they are attributed by different mechanisms
//
// The check used to look for a bare `PlatformId` and report healthy when it found one. That
// covers the tenant's DATASTORES and nothing else, because attribution by resource tag requires
// a resource that can carry a tag — and a Bedrock model invocation is not one. No `resourceTags/`
// key is ever populated on an invocation line.
//
// AWS attributes model spend by CALLING IDENTITY instead. Tags on the IAM user or role appear in
// CUR under a separate prefix, `iamPrincipal/<key>`, alongside `resourceTags/<key>`, and the two
// are activated separately — the Billing console has an IAM-principal tag type you filter for.
// So the two keys attribute disjoint halves of the bill, and the half that resource tags cannot
// see is the dominant cost. A check that looks only at the bare key reports healthy on an
// account whose model spend is entirely unattributed.
//
// # And the IAM-principal half CANNOT be activated before the install
//
// "For Amazon Bedrock, tags only appear for activation after the IAM principal with the tags has
// made at least one API call" — then up to 24 hours for the key to appear, and up to 24 more to
// activate. So unlike the resource-tag half, this one is not a thing to do first. It is a thing
// to do promptly AFTER the first invocations, and telling an operator to activate it now would
// send them looking for a key that cannot exist yet.
//
// It also requires CUR 2.0: "If you are using the legacy CUR format, IAM principal fields will
// not be available." landing-zone's org-cost models a CUR 2.0 Data Export with
// INCLUDE_IAM_PRINCIPAL_DATA, which is the layer that satisfies this.
func CheckCostAllocationTags(ctx context.Context, env *Env) doctor.Result {
	const name = "cost allocation"

	if !env.Cfg.AgentPlatform.Enabled() {
		return ok(name, "agent platform disabled — no budget pipeline to feed")
	}

	// Cost Explorer is global and only answers in us-east-1, whatever the deployment region is.
	raw, err := env.Run.Capture(ctx, "aws", "ce", "list-cost-allocation-tags",
		"--status", "Active", "--query", "CostAllocationTags[].TagKey",
		"--region", "us-east-1", "--output", "text")
	if err != nil {
		return warn(name, "could not read cost allocation tags")
	}
	active := map[string]bool{}
	// A key may come back bare or carrying its CUR column prefix. Both forms are recorded so the
	// resource-tag half and the IAM-principal half can be told apart rather than collapsed —
	// collapsing them is what made this check report healthy over unattributed model spend.
	iamPrincipal := map[string]bool{}
	for _, k := range strings.Fields(raw) {
		active[k] = true
		if rest, ok := strings.CutPrefix(k, "iamPrincipal/"); ok {
			iamPrincipal[rest] = true
		}
	}

	var missing []string
	for _, k := range []string{"PlatformId", "Repository"} {
		if !active[k] {
			missing = append(missing, k)
		}
	}

	// The model-spend half. Reported whether or not the resource-tag half is healthy, because
	// "PlatformId is active" is precisely the observation that used to hide it.
	modelSpend := ""
	if !iamPrincipal["PlatformId"] && !active["iamPrincipal/PlatformId"] {
		modelSpend = "No iamPrincipal/PlatformId is active, so MODEL spend is unattributed — and " +
			"model spend is the dominant cost. A Bedrock invocation is not a taggable resource, so " +
			"no resourceTags/ key is ever populated on one; AWS attributes it by calling identity " +
			"instead, and IAM-principal tags are activated separately from resource tags (filter " +
			"for the IAM principal tag type in Billing → Cost allocation tags). This one cannot be " +
			"done yet if nothing has invoked a model: the key only appears for activation after a " +
			"tagged principal has made at least one call, then takes up to 24h to appear and up to " +
			"24h more to activate. It also needs CUR 2.0 — the legacy format carries no IAM " +
			"principal fields at all. Do it once the platform is serving traffic, not before."
	}

	switch {
	case len(missing) == 0 && modelSpend == "":
		return ok(name, "PlatformId and Repository are active, and iamPrincipal/PlatformId covers model spend")
	case len(missing) == 0:
		return warn(name, "PlatformId and Repository are active, which covers the tenant's "+
			"datastores. "+modelSpend)
	}

	detail := fmt.Sprintf(
		"%s %s not activated as cost allocation tag(s), so %s NULL in CUR. The budget reconciler "+
			"filters on resource_tags_user_platform_id, and Repository is what separates this "+
			"deployment's spend from anything else in the account. Activation is payer-level and "+
			"NOT retroactive — activating after the first tenant exists loses that tenant's early "+
			"spend permanently, so it wants doing now rather than when a report looks wrong. "+
			"Activate under Billing → Cost allocation tags.",
		strings.Join(missing, " and "),
		map[bool]string{true: "is", false: "are"}[len(missing) == 1],
		map[bool]string{true: "it is", false: "they are"}[len(missing) == 1])
	if modelSpend != "" {
		detail += " SEPARATELY: " + modelSpend
	}
	return warn(name, detail)
}
