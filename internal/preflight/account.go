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
// account fills up with everything else the startup does. The account this was written against
// holds three estates: the deployment, a personal site estate, and a mail domain, with 12 live
// Route53 zones, 25 S3 buckets and an already-configured Bedrock logging singleton between them.
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

// plannedBucket is a bucket this config will try to create, and the component that owns it.
//
// persistent marks the two Terraform state backends. Their existence is not wreckage — it is
// the normal steady state, and the expected one from the second install onwards. Both are
// created idempotently behind a head-bucket guard (landing-zone scripts/init-backend-aws.sh:12,
// phases/agentplatform.go:224), neither is ever deleted by rackctl, and both are deliberately
// SHARED across every environment in the account, differing only by state key.
//
// The distinction is load-bearing rather than cosmetic: treating them like component buckets
// made this check fail on the account it was written against, and it would have failed on every
// account that had ever run an install. A preflight that fires on the healthy steady state is
// worse than no preflight, because it teaches the operator to skip it.
//
// They still get the other two checks. A name over 63 characters is still fatal, and a 403 —
// the name existing in somebody else's account — is MORE fatal here than anywhere else, because
// without a backend nothing can be applied at all.
type plannedBucket struct {
	name, owner string
	persistent  bool
}

// plannedBuckets returns every S3 bucket name this config would claim.
//
// Composed from the components' own expressions rather than guessed:
//
//	agent-iam       artifacts.tf:50-52   <cluster>-<account>-<region>-{model-artifacts,eval-reports,access-logs}
//	cluster-addons  main.tf:37 + s3.tf   <cluster>-<account>-<region>-{velero,loki,tempo,argo-workflows}
//	model-import    main.tf:65           <environment>-<account>-<region>-model-import
//	bedrock         main.tf:4,17,72      <cluster>-bedrock-{access-logs,invocations}-<account>
//	cost-pipeline   main.tf:16,29,94,227 <cluster>-cost-{access-logs,cur,athena}-<account>
//	backends        init-backend-aws.sh:9 / agentplatform.go:214
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
		{name: fmt.Sprintf("%s-%s-tfstate", acct, region), owner: "terraform backend", persistent: true},
	}
	for _, s := range []string{"velero", "loki", "tempo", "argo-workflows"} {
		bs = append(bs, plannedBucket{name: fmt.Sprintf("%s-%s-%s-%s", cluster, acct, region, s), owner: "cluster-addons"})
	}
	if cfg.AgentPlatform.Enabled() {
		for _, s := range []string{"model-artifacts", "eval-reports", "access-logs"} {
			bs = append(bs, plannedBucket{name: fmt.Sprintf("%s-%s-%s-%s", cluster, acct, region, s), owner: "agent-iam"})
		}
		bs = append(bs,
			plannedBucket{name: fmt.Sprintf("eks-agent-platform-tfstate-%s-%s", acct, region), owner: "agent-platform backend", persistent: true},
			plannedBucket{name: fmt.Sprintf("%s-bedrock-access-logs-%s", cluster, acct), owner: "bedrock"},
			plannedBucket{name: fmt.Sprintf("%s-bedrock-invocations-%s", cluster, acct), owner: "bedrock"},
			plannedBucket{name: fmt.Sprintf("%s-cost-access-logs-%s", cluster, acct), owner: "cost-pipeline"},
			plannedBucket{name: fmt.Sprintf("%s-cost-cur-%s", cluster, acct), owner: "cost-pipeline"},
			plannedBucket{name: fmt.Sprintf("%s-cost-athena-%s", cluster, acct), owner: "cost-pipeline"},
		)
		if cfg.AgentPlatform.ModelImport {
			bs = append(bs, plannedBucket{
				name: fmt.Sprintf("%s-%s-%s-model-import", envName, acct, region), owner: "model-import"})
		}
	}
	return bs
}

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

	var tooLong, taken, ours []string
	for _, b := range planned {
		if len(b.name) > 63 {
			tooLong = append(tooLong, fmt.Sprintf("%s (%d chars, %s)", b.name, len(b.name), b.owner))
			continue
		}
		if owned[b.name] {
			// A state backend that already exists is the steady state, not a collision.
			if !reapply && !b.persistent {
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

	var existing []string
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == want {
			kind := "public"
			if len(f) >= 3 && strings.EqualFold(f[2], "true") {
				kind = "private"
			}
			existing = append(existing, fmt.Sprintf("%s (%s)", f[1], kind))
		}
	}
	if len(existing) == 0 {
		return ok(name, want+" has no zone in this account — dns will mint it")
	}
	return fail(name, fmt.Sprintf(
		"%s already has %d hosted zone(s) in this account — %s. The dns component runs in create "+
			"mode, so it would mint ANOTHER one; AWS allows that, and the registrar's NS delegation "+
			"points at only one. external-dns would then publish into a zone nothing resolves, and a "+
			"later `rackctl destroy` would delete a zone that may be the live one. If this domain is "+
			"already delegated, dns.hostedZone is not the right way to reach it.",
		want, len(existing), strings.Join(existing, ", ")))
}

// ─────────────────────────── bedrock's account singleton ───────────────────────────

// CheckBedrockLogging asserts this run will not silently take over another environment's Bedrock
// invocation logging.
//
// aws_bedrock_model_invocation_logging_configuration (eks-agent-platform
// components/bedrock/main.tf:238) has no name and no identifier: the Bedrock API holds EXACTLY
// ONE configuration per account per region. The component's buckets are correctly cluster-scoped;
// the configuration that points at them cannot be.
//
// Under rackctl every environment shares one account, so applying development overwrites
// production's logging destination and tearing development down deletes the singleton outright —
// leaving production with no invocation logging at all. Both applies are green. Invocation
// logging is the signal every budget decision reads, so this is the org's own recurring class
// sitting on the flagship feature.
//
// Filed upstream as ledger O14. Until it lands, refusing is the only honest move rackctl has:
// there is no TF_VAR that makes the resource per-environment, because the API has no such axis.
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

	// The name this run's own bedrock component would set. If it matches, the singleton is
	// already ours and re-applying is a no-op rather than a takeover.
	mine := fmt.Sprintf("%s-bedrock-invocations-%s", clusterName(cfg), cfg.Cloud.AccountID)
	if bucket == mine {
		return ok(name, "the invocation logging singleton is already this cluster's")
	}
	return fail(name, fmt.Sprintf(
		"Bedrock invocation logging in %s is already configured, delivering to %q — and this run's "+
			"bedrock component would repoint it at %q. There is exactly ONE such configuration per "+
			"account per region: it has no name and nothing to scope it by, so applying here takes it "+
			"over silently and a later teardown DELETES it rather than restoring the previous owner. "+
			"Invocation logging is what every budget decision reads, so the environment that loses it "+
			"stops being able to bill or cap anything, with nothing going red. Set agentPlatform.enable: "+
			"false, or use an account that does not already have this set. (Upstream fix: ledger O14.)",
		cfg.Cloud.Region, bucket, mine))
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
// PlatformId is what the budget reconciler filters CUR on. Repository is what tells this
// deployment's spend apart from anything else in the account — and in a shared account that
// distinction is the difference between a tenant's bill and somebody's website.
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
	for _, k := range strings.Fields(raw) {
		active[k] = true
	}

	var missing []string
	for _, k := range []string{"PlatformId", "Repository"} {
		if !active[k] {
			missing = append(missing, k)
		}
	}
	if len(missing) == 0 {
		return ok(name, "PlatformId and Repository are active cost allocation tags")
	}
	return warn(name, fmt.Sprintf(
		"%s %s not activated as cost allocation tag(s), so %s NULL in CUR. The budget reconciler "+
			"filters on resource_tags_user_platform_id, and Repository is what separates this "+
			"deployment's spend from anything else in the account. Activation is payer-level and "+
			"NOT retroactive — activating after the first tenant exists loses that tenant's early "+
			"spend permanently, so it wants doing now rather than when a report looks wrong. "+
			"Activate under Billing → Cost allocation tags.",
		strings.Join(missing, " and "),
		map[bool]string{true: "is", false: "are"}[len(missing) == 1],
		map[bool]string{true: "it is", false: "they are"}[len(missing) == 1]))
}
