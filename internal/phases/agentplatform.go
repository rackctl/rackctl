package phases

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rackctl/rackctl/internal/engine"
)

// apComponent is one component of the eks-agent-platform tree together with the CARDINALITY of
// its root, which is the thing this file exists to get right.
//
// Two components own objects AWS keeps exactly one of per account and region — the Bedrock
// invocation-logging configuration, and the Cost and Usage Report. Neither can be modelled per
// environment without three roots fighting over one object, so upstream gave them their own
// roots under live/org and left the rest per cluster. A single path template cannot address
// both, which is how this phase came to abort before applying anything: it looked for
// live/<env>-platform/cost-pipeline, a directory that no longer exists.
type apComponent struct {
	name string
	// account marks a root applied ONCE for the account, at live/org/<name>, rather than once
	// per environment. It changes three things: the path, the variables it is handed, and
	// whether a teardown of one environment is allowed to remove it at all.
	account bool
}

// agentPlatformComponents is the apply order. Destroy runs it in reverse, minus the account
// roots unless a teardown explicitly asks for them.
//
// The ordering is two chains joined at one point:
//
//	bedrock-account → cost-pipeline        the account chain, applied once
//	bedrock → … → cost-access              the cluster chain
//
// bedrock-account first, because it is the only root here with no upstream dependency and
// everything downstream reads what it publishes. cost-pipeline follows it: the account root
// reads bedrock-account's invocation log-group name from SSM
// (/eks-agent-platform/org/bedrock-account/invocation_log_group) and subscribes to the log group
// bedrock-account owns.
//
// That ordering used to be expressed as a terragrunt `dependency "bedrock"` block, and the
// comment here still said so long after it stopped being true. The dependency is real; the
// mechanism is SSM now, which means terragrunt will NOT enforce it — a wrong order fails at
// apply against a parameter that does not exist yet, rather than being reordered for us. So the
// order in this slice is load-bearing in a way it was not before.
//
// cost-access goes last in the cluster chain because it is the join: it reads the account
// contract cost-pipeline publishes under /eks-agent-platform/org/cost-pipeline/* AND landing-zone
// agent-iam's operator role under /eks-agent-platform/<cluster>/agent-iam/*, then republishes the
// account values under this cluster's key so the operator can resolve them by the same
// cluster-scoped path it uses for everything else. It holds no substrate of its own — just the
// IAM grant and that republish.
//
// batch-runtime is deliberately absent. It is a component with no live root in any environment,
// so there is nothing here to apply — see assertAgentPlatformRoots, which says so rather than
// letting terragrunt discover it.
func agentPlatformComponents() []apComponent {
	return []apComponent{
		{name: "bedrock-account", account: true},
		{name: "cost-pipeline", account: true}, // reads bedrock-account's log group over SSM
		{name: "bedrock"},
		{name: "agent-egress"},
		{name: "accelerator-pools"},
		{name: "eval-runtime"},
		{name: "kill-switch"},
		{name: "cost-access"}, // last: joins the account contract to this cluster's operator
	}
}

// agentPlatformComponentNames is the display list, for notes that name what is being applied.
func agentPlatformComponentNames(comps []apComponent) []string {
	names := make([]string, 0, len(comps))
	for _, c := range comps {
		names = append(names, c.name)
	}
	return names
}

// agentPlatformDir is the terragrunt path for one component of that tree.
//
// Per cluster the layout is live/<environment>-platform/<component> — note the "-platform"
// suffix on the environment directory, which is this tree's own convention and not
// landing-zone's. Account roots live at live/org/<component> instead: `org` is the reserved
// account-scope token from nanohype/standards/resource-naming.json, occupying the environment
// slot rather than adding a tier above it, and live/org/env.hcl pins `environment = "org"` for
// everything under it.
func agentPlatformDir(st *engine.State, c apComponent) string {
	if c.account {
		return fmt.Sprintf("terraform/live/org/%s", c.name)
	}
	return fmt.Sprintf("terraform/live/%s-platform/%s", st.Config.Environment, c.name)
}

// agentPlatformEnv builds the TF_VARs this tree needs from what rackctl captured while
// applying landing-zone.
//
// The tree's own README states the contract: its landing-zone identifiers "are passed in as
// TF_VAR_* environment variables by the orchestrator". rackctl is the orchestrator, and until
// now it passed nothing — so nothing in this tree had ever been applied by rackctl at all.
//
// Two variables are deliberately NOT set here, and that is the sharpest thing in this
// function. An ambient TF_VAR beats a leaf's inputs, so injecting a value that the leaf
// already gets correctly does not add belt-and-braces — it silently replaces a right answer
// with a guessed one:
//
//   - allowed_regions is a POLICY value, pinned identically in all three eval-runtime leaves
//     and mirrored by the org's SCP guardrail. rackctl has no opinion about which regions an
//     org permits, and inventing one here would quietly widen or narrow a control.
//   - bedrock_invocation_log_group is already wired by each cost-pipeline leaf's
//     `dependency "bedrock"` block, which resolves the real value from bedrock's state.
//     Overriding it with a name rackctl composed would point the subscription filter at a log
//     group that may not exist.
func agentPlatformEnv(st *engine.State) ([]string, error) {
	// cluster_name is the FULL EKS cluster name here, not the base. That is the opposite of
	// what rackctl passes landing-zone's `cluster` component, and getting it backwards is
	// silent: every IAM role, log group and SSM path this tree mints would carry "platform"
	// where it should carry "development-platform".
	//
	// landing-zone's `cluster` component is the ONE place that takes a base and composes
	// <environment>-<base> itself; every other component downstream of it — in either repo —
	// receives the composed name. This tree follows that same downstream convention, so it is
	// not the odd one out.
	env := []string{"TF_VAR_cluster_name=" + st.Config.ClusterName()}

	// AWS_ACCOUNT_ID, not TF_VAR_. env.hcl reads it with get_env() to build the state bucket
	// name, which terragrunt resolves at PARSE time — before variables exist. rackctl already
	// sets TERRAGRUNT_ACCOUNT_ID for landing-zone, and this tree does not read that name.
	env = append(env, "AWS_ACCOUNT_ID="+st.Config.Cloud.AccountID)

	// Two variables, two outputs — one each. This used to read a single output and assign it
	// twice, on the reasoning that there was no separate "logs" and "data" CMK to read. That was
	// true when it was written and stopped being true with landing-zone#205.
	//
	// The secrets component now takes `separate_logs_key`. It defaults to false, which mints one
	// key and publishes it as BOTH kms_key_arn and logs_kms_key_arn — so reading the log-path
	// output changes nothing today. Set it true and the log-path grants (logs.<region>
	// .amazonaws.com and bedrock.amazonaws.com) MOVE to a second key rather than being copied,
	// deliberately: a separate logs key that the secrets key still admitted would let a log
	// group encrypt under either and enforce nothing.
	//
	// Which is what makes this worth landing before anyone flips the flag. An installer still
	// passing the data key as logs_kms_key_arn does not silently encrypt logs under the wrong
	// key — it fails at CreateLogGroup with a KMS error. Fail-closed and loud, but still a
	// failed install rather than a caught misconfiguration, and reading the right output avoids
	// it entirely. Ledger O25.
	dataKMS, err := needOutput(st, "kms_key_arn", "secrets")
	if err != nil {
		return nil, err
	}
	env = append(env,
		"TF_VAR_logs_kms_key_arn="+logsKMSKey(st, dataKMS),
		"TF_VAR_data_kms_key_arn="+dataKMS)

	vpc, err := needOutput(st, "vpc_id", "network")
	if err != nil {
		return nil, err
	}
	privSubnets, err := needOutput(st, "private_subnet_ids", "network")
	if err != nil {
		return nil, err
	}
	env = append(env,
		"TF_VAR_vpc_id="+vpc,
		// Already compact JSON — tf.ParseOutputs returns non-string outputs that way, which is
		// exactly the shape TF_VAR_ wants for a list(string). Do not comma-join.
		"TF_VAR_private_subnet_ids="+privSubnets)

	// route_table_ids has no matching landing-zone output; the network component exports the
	// two halves separately. agent-egress wants BOTH, because it creates S3 and DynamoDB
	// GATEWAY endpoints, and a gateway endpoint installs its prefix-list route into every
	// route table it is associated with. landing-zone's own S3 gateway endpoint associates
	// with the flattened pair, so matching that is what makes the two trees agree about which
	// subnets can reach S3 without NAT.
	rtIDs, err := routeTableIDs(st)
	if err != nil {
		return nil, err
	}
	env = append(env, "TF_VAR_route_table_ids="+rtIDs)

	sg, err := needOutput(st, "cluster_security_group_id", "cluster")
	if err != nil {
		return nil, err
	}
	env = append(env, "TF_VAR_cluster_security_group_id="+sg)

	// node_role_name is deliberately NOT sent, and this is a deletion rather than an omission.
	//
	// accelerator-pools used to attach an inline ec2:Describe* policy to the Karpenter node role
	// for the AWS Neuron device plugin's topology discovery. There is no Neuron device plugin —
	// eks-gitops installs the GPU Operator and the NVIDIA DRA driver and nothing else — so the
	// whole Neuron half went upstream, and `var.node_role_name` and the data source resolving it
	// went with it (accelerator-pools/variables.tf:11 records the absence deliberately).
	//
	// tofu ignores a TF_VAR_ naming a variable no root declares, so this was inert rather than
	// broken. What was not inert was the `needOutput(st, "karpenter_node_role_name", "cluster")`
	// behind it: a hard requirement that failed the whole phase before applying anything, to
	// produce a value nothing reads. Ledger O24.

	return env, nil
}

// agentPlatformAccountEnv builds the TF_VARs the two account-scoped roots need.
//
// A separate function rather than a filter over the cluster set, because the two are different
// in kind rather than in degree. An account root is applied once for the account and must
// produce the same thing whichever environment's install happens to reach it first — so the
// interesting question is not "which of these variables does it use" but "which of them is it
// safe to hand something environment-specific".
//
// Deliberately NOT sent:
//
//   - cluster_name. Neither account component declares it, and live/root.hcl merges it into a
//     root's inputs only when the root has one. Sending it would be inert today and is exactly
//     the kind of inert-until-it-isn't input that produces a resource named for a cluster in a
//     place where there is no cluster.
//   - environment. Both components declare it with `default = "org"` and a validation block
//     pinning it to that literal. An ambient TF_VAR beats a leaf's inputs (ledger S1), so
//     sending the config's environment here would rename every account-scoped bucket —
//     development-<acct>-<region>-cost-* rather than org-… — except that upstream's validation
//     refuses it first. Relying on somebody else's guard is not the same as not making the
//     mistake, so it is not sent.
//
// What IS sent is the pair of CMK ARNs, and there is a real hazard in them worth stating where
// it can be read. landing-zone mints one secrets CMK per ENVIRONMENT (there is no `org` secrets
// root), so whichever environment installs first hands the account its cost and log key, and a
// later install from a different environment repoints it. That is upstream's shape rather than
// something rackctl can fix from here — see noteAccountScopedApply, which says so at apply time
// instead of leaving it to be discovered.
func agentPlatformAccountEnv(st *engine.State) ([]string, error) {
	// AWS_ACCOUNT_ID, not a TF_VAR: live/org/env.hcl reads it with get_env() to build the state
	// bucket name, which terragrunt resolves at PARSE time, before variables exist.
	env := []string{"AWS_ACCOUNT_ID=" + st.Config.Cloud.AccountID}

	dataKMS, err := needOutput(st, "kms_key_arn", "secrets")
	if err != nil {
		return nil, err
	}
	// bedrock-account takes only logs_kms_key_arn and does not declare a data key; cost-pipeline
	// takes both. Sending both to both is inert on the one that does not declare it, and keeps
	// the account chain from needing a per-component variable table.
	env = append(env,
		"TF_VAR_logs_kms_key_arn="+logsKMSKey(st, dataKMS),
		"TF_VAR_data_kms_key_arn="+dataKMS)

	return env, nil
}

// logsKMSKey resolves the log-path CMK, falling back to the data key when the secrets state on
// disk predates the output that publishes it.
//
// The fallback is not a guess, and the argument for it is what makes it safe: logs_kms_key_arn
// is published in BOTH modes, so a state that does not carry it was written by a module version
// that could not separate the two keys — which means there is exactly one key, and the data key
// IS the log key. A state old enough to be missing the output is old enough that the fallback
// is the only value it could ever have had.
//
// It earns its place on the DESTROY path. `rackctl destroy` starts cold and reads these outputs
// back out of a state nothing has re-applied, so a hard needOutput here would refuse to tear
// down any platform installed before landing-zone#205 — and it would refuse at the point where
// the operator has already decided to spend nothing more. A teardown that cannot run is the
// exact failure this is meant to prevent, not an acceptable price for strictness.
func logsKMSKey(st *engine.State, dataKMS string) string {
	if v := st.Outputs["logs_kms_key_arn"]; v != "" {
		return v
	}
	// A dry-run captures nothing, so name the output this WOULD read rather than echoing the
	// data key's placeholder twice and hiding that there are two reads.
	if st.Runner.DryRun {
		return "<secrets.logs_kms_key_arn>"
	}
	note(st, "landing-zone's secrets state publishes no logs_kms_key_arn, so it predates the "+
		"separate-logs-key change — using the secrets key for the log path, which is the value "+
		"that state's own module would have published there")
	return dataKMS
}

// needOutput reads a captured terragrunt output, failing with the component that produces it.
//
// captureOutputs is best-effort and silently returns on any error, which is right for the
// advisory uses it was written for. Here the values are REQUIRED: a missing one produces an
// apply with an empty TF_VAR, and an empty KMS ARN or VPC id does not fail fast — it fails
// somewhere inside a provider call with a message naming neither rackctl nor the component
// that should have produced it.
func needOutput(st *engine.State, key, producer string) (string, error) {
	v := st.Outputs[key]
	// A dry-run captures nothing — captureOutputs returns immediately without --apply — so
	// every one of these would be missing, and treating that as an error would make a PLAN
	// fail, and (worse) trip the rollback. A plan's job is to say what would happen. The
	// placeholder is deliberately not a plausible value: it appears in the printed TF_VAR so
	// the reader can see this is the shape of the run, not a resolved one.
	if v == "" && st.Runner.DryRun {
		return "<" + producer + "." + key + ">", nil
	}
	if v == "" {
		return "", fmt.Errorf("the agent-platform substrate needs %q, which landing-zone's %q component "+
			"produces, and it was not captured. rackctl reads it with `terragrunt output -json` after "+
			"applying %s — so either that component did not apply, or its outputs changed shape. "+
			"Nothing in eks-agent-platform/terraform has been applied yet, so nothing needs unwinding",
			key, producer, producer)
	}
	return v, nil
}

// routeTableIDs concatenates the private and public route table lists into the single JSON
// array agent-egress takes. Public may legitimately be empty — an adopted VPC re-exports no
// public route tables, and a private-only cluster has none — so only the private half is
// required.
func routeTableIDs(st *engine.State) (string, error) {
	priv, err := needOutput(st, "private_route_table_ids", "network")
	if err != nil {
		return "", err
	}
	if st.Runner.DryRun && strings.HasPrefix(priv, "<") {
		return `["<network.private_route_table_ids>","<network.public_route_table_ids>"]`, nil
	}
	var ids []string
	if err := json.Unmarshal([]byte(priv), &ids); err != nil {
		return "", fmt.Errorf("landing-zone's private_route_table_ids output is not a JSON list (%q): %w", priv, err)
	}
	if pub := st.Outputs["public_route_table_ids"]; pub != "" {
		var pubIDs []string
		if err := json.Unmarshal([]byte(pub), &pubIDs); err != nil {
			return "", fmt.Errorf("landing-zone's public_route_table_ids output is not a JSON list (%q): %w", pub, err)
		}
		ids = append(ids, pubIDs...)
	}
	out, err := json.Marshal(ids)
	if err != nil {
		return "", fmt.Errorf("encoding route_table_ids: %w", err)
	}
	return string(out), nil
}

// ensureAgentPlatformBackend creates the state bucket this tree writes to.
//
// It is a DIFFERENT bucket from landing-zone's: eks-agent-platform-tfstate-<account>-<region>
// versus <account>-<region>-tfstate. Nothing creates it. rackctl's identity phase runs
// landing-zone's scripts/init-backend-aws.sh, which hardcodes landing-zone's name, and the
// only record of this one is a copy-paste snippet in the tree's README.
//
// Terragrunt will not create it either. Auto-bootstrap of the S3 backend stopped being the
// default, and the opt-in flag is not one rackctl passes — so the first apply fails on a
// missing bucket, after everything else in the run has succeeded.
//
// Versioned and encrypted, matching what the README documents and what landing-zone's own
// script does for its bucket. Idempotent via head-bucket, so a re-run costs one API call.
func ensureAgentPlatformBackend(ctx context.Context, st *engine.State) error {
	bucket := fmt.Sprintf("eks-agent-platform-tfstate-%s-%s", st.Config.Cloud.AccountID, st.Config.Cloud.Region)
	region := st.Config.Cloud.Region

	if st.Runner.DryRun {
		note(st, "agent-platform state backend: would ensure s3://%s exists (versioned, encrypted). "+
			"This is a SECOND state bucket — landing-zone uses %s-%s-tfstate — and nothing else creates it",
			bucket, st.Config.Cloud.AccountID, region)
		return nil
	}

	if _, err := st.Runner.Capture(ctx, "aws", "s3api", "head-bucket", "--bucket", bucket); err == nil {
		note(st, "agent-platform state backend: s3://%s already exists", bucket)
		return nil
	}

	note(st, "agent-platform state backend: creating s3://%s", bucket)
	if err := st.Runner.Run(ctx, "aws", "s3api", "create-bucket", "--bucket", bucket,
		"--region", region, "--create-bucket-configuration", "LocationConstraint="+region); err != nil {
		return fmt.Errorf("creating the agent-platform state bucket %s: %w", bucket, err)
	}
	if err := st.Runner.Run(ctx, "aws", "s3api", "put-bucket-versioning", "--bucket", bucket,
		"--versioning-configuration", "Status=Enabled"); err != nil {
		return fmt.Errorf("enabling versioning on %s: %w", bucket, err)
	}
	if err := st.Runner.Run(ctx, "aws", "s3api", "put-bucket-encryption", "--bucket", bucket,
		"--server-side-encryption-configuration",
		`{"Rules":[{"ApplyServerSideEncryptionByDefault":{"SSEAlgorithm":"AES256"}}]}`); err != nil {
		return fmt.Errorf("enabling encryption on %s: %w", bucket, err)
	}
	if err := st.Runner.Run(ctx, "aws", "s3api", "put-public-access-block", "--bucket", bucket,
		"--public-access-block-configuration",
		"BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true"); err != nil {
		return fmt.Errorf("blocking public access on %s: %w", bucket, err)
	}
	return nil
}

// applyAgentPlatform applies the eks-agent-platform terraform tree, in order.
//
// This closes the largest gap rackctl had. Without it a rackctl-installed cluster ran the
// agent operator with no governance substrate underneath: no Bedrock invocation logging or
// baseline guardrail, so every budget decision read a signal that did not exist; no
// kill-switch EventBridge bus or Step Functions machine, so a budget breach detached nothing
// and the SLO burn-rate rules had nowhere to route; and no cost-pipeline CUR/Athena/Glue,
// which is what the operator's budget reconciler queries — leaving it with no data source at
// all.
func applyAgentPlatform(ctx context.Context, st *engine.State) error {
	clusterEnv, err := agentPlatformEnv(st)
	if err != nil {
		return err
	}
	accountEnv, err := agentPlatformAccountEnv(st)
	if err != nil {
		return err
	}
	if err := assertAgentPlatformRoots(st); err != nil {
		return err
	}
	if err := ensureAgentPlatformBackend(ctx, st); err != nil {
		return err
	}

	// The tree lives in its own checkout, so the runner has to be repointed for the duration
	// and put back afterwards — the phases that follow expect landing-zone. Same for the
	// environment: these variables belong to this tree and must not leak into anything after
	// it, which is the rule componentEnv exists to enforce one repo over.
	prevDir, prevEnv := st.Runner.Dir, st.Runner.Env
	st.Runner.Dir = st.Repos.AgentPlatform
	defer func() { st.Runner.Dir, st.Runner.Env = prevDir, prevEnv }()

	if st.Runner.DryRun {
		note(st, "agent-platform substrate: (apply) reads kms_key_arn and logs_kms_key_arn from "+
			"landing-zone's secrets; vpc_id, private_subnet_ids and both route-table lists from "+
			"network; cluster_security_group_id from cluster — then hands them to this tree as "+
			"TF_VAR_*. The <placeholders> below are those reads, not values")
	}
	noteAgentPlatformTeardown(st)
	noteAccountScopedApply(st)

	for _, c := range agentPlatformComponents() {
		// The environment is rebuilt per component rather than once for the loop, because the
		// account roots must not receive the cluster set. Assigning a fresh slice each time
		// also keeps one component's variables from accumulating onto the next.
		vars := clusterEnv
		if c.account {
			vars = accountEnv
		}
		st.Runner.Env = append(append([]string(nil), prevEnv...), vars...)

		dir := agentPlatformDir(st, c)
		if err := st.Runner.Run(ctx, "terragrunt", "--working-dir", dir, "--non-interactive", "init"); err != nil {
			return fmt.Errorf("agent-platform %s: init: %w", c.name, err)
		}
		if err := st.Runner.Run(ctx, "terragrunt", "--working-dir", dir, "--non-interactive",
			"apply", "-auto-approve"); err != nil {
			return fmt.Errorf("agent-platform %s: apply: %w", c.name, err)
		}
	}
	return nil
}

// noteAccountScopedApply discloses that two of these roots are not this environment's.
//
// Applying them is correct and idempotent — they are the same objects whichever environment
// reaches them — but two consequences are invisible from inside a single-environment install
// and expensive to discover from outside one.
func noteAccountScopedApply(st *engine.State) {
	note(st, "agent-platform substrate: bedrock-account and cost-pipeline apply at live/org, ONCE "+
		"for the account rather than once per environment. AWS keeps exactly one Bedrock "+
		"invocation-logging configuration and one Cost and Usage Report per account, so these are "+
		"shared with every other environment installed here")
	note(st, "agent-platform substrate: NOTE — the account's cost and log CMK comes from THIS "+
		"environment's landing-zone secrets component, because landing-zone mints one secrets key "+
		"per environment and there is no `org` secrets root. Installing a second environment here "+
		"later repoints the account's key at that environment's, and objects already written stay "+
		"encrypted under this one — so this key must not be destroyed while the account pipeline "+
		"holds data it wrote")
}

// noteAgentPlatformTeardown says, at apply time, which of these components cannot be taken
// back down — the same disclosure druid gets, and for the same reason.
//
// bedrock and cost-pipeline own four S3 buckets between them with no force_destroy:
// bedrock's access-logs, and cost-pipeline's access-logs, cur and athena-results. Two are
// versioned. All four are written to in the ordinary course of the platform running — S3
// server-access logs land from the first PUT — so after any traffic a destroy fails
// BucketNotEmpty. The lifecycle expiries do not save it: expiration on a versioned bucket
// writes delete markers, which are themselves current versions.
//
// Object Lock is NOT the blocker, which is worth saying because it looks like it should be:
// the leaves pin GOVERNANCE mode, and bedrock's own force_destroy is
// `var.object_lock_mode != "COMPLIANCE"`, so it resolves true. It does quietly require
// s3:BypassGovernanceRetention on the caller, which rackctl neither declares nor checks.
func noteAgentPlatformTeardown(st *engine.State) {
	note(st, "agent-platform substrate: applying %s",
		strings.Join(agentPlatformComponentNames(agentPlatformComponents()), ", "))
	note(st, "agent-platform substrate: NOTE — the account-scoped buckets take writes from the first "+
		"PUT (S3 server-access logs land immediately), so a teardown meets BucketNotEmpty unless "+
		"force_destroy is landed in state first. cost-pipeline now accepts force_destroy_buckets and "+
		"bedrock-account derives it from object_lock_mode != COMPLIANCE, which live/org pins to "+
		"GOVERNANCE for exactly this reason — so `rackctl destroy --account-scoped --force-buckets` "+
		"can now take them down, where before this had to be done by hand. Bedrock's invocations "+
		"bucket still carries per-object GOVERNANCE retention, so that path needs "+
		"s3:BypassGovernanceRetention on the caller")
}

// assertAgentPlatformRoots verifies each component has a live root in the checkout, for the
// same reason the landing-zone guard does: terragrunt exits 1 immediately on a directory with
// no terragrunt.hcl, and discovering that here costs nothing while discovering it mid-apply
// costs whatever has already been applied.
//
// It is wrapped in NoRollbackError because it runs before this tree has applied anything, and
// the engine's rollback is not a no-op on an empty run — it sweeps IAM roles and, once the
// cluster phase has repointed the kubeconfig, deletes every Platform and PVC it can see.
//
// One directory is deliberately tolerated: live/production-platform/agent-iam holds a
// .terraform.lock.hcl and a cache but no terragrunt.hcl. It is a stray from a component that
// moved to landing-zone. Nothing here enumerates directories — the component list is explicit
// — so it is inert, and this comment exists so the next person to write a directory walk
// knows it is there.
func assertAgentPlatformRoots(st *engine.State) error {
	var missing []apComponent
	for _, c := range agentPlatformComponents() {
		if _, err := os.Stat(filepath.Join(st.Repos.AgentPlatform, agentPlatformDir(st, c), "terragrunt.hcl")); err != nil {
			missing = append(missing, c)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	names := strings.Join(agentPlatformComponentNames(missing), ", ")
	if st.Runner.DryRun {
		note(st, "PLAN ONLY: this eks-agent-platform checkout has no live root for %s. A dry-run does "+
			"not pull, so the tree on disk predates whatever upstream has now", names)
		return nil
	}
	return &engine.NoRollbackError{Err: fmt.Errorf(
		"eks-agent-platform has no live root for %s (looked for %s).\n\n"+
			"That tree authors its roots at two cardinalities: per environment under "+
			"live/<environment>-platform, and once per account under live/org for the two components "+
			"that own account+region singletons. batch-runtime — the one component with no root in ANY "+
			"environment — is excluded here for exactly this reason. If a root has moved or gone "+
			"missing upstream, this is the report; nothing in the agent-platform tree has been "+
			"applied, so nothing needs unwinding",
		names, agentPlatformDir(st, missing[0]))}
}

// DestroyAgentPlatform tears the tree down in reverse, and MUST run before landing-zone's
// components are destroyed.
//
// The ordering is not a preference. Ten unguarded `data "aws_ssm_parameter"` blocks across
// these components resolve landing-zone values — agent-iam's operator role and tenant paths,
// observability's alert topic ARNs — and Terraform evaluates data sources during a DESTROY
// plan as well as an apply. So destroying landing-zone's agent-iam or observability first
// leaves five of these six leaves unable to plan their own destroy at all: they fail at
// parameter resolution, before deleting anything. accelerator-pools and eval-runtime add a
// second constraint — they own EKS Pod Identity associations, so they must go before the
// cluster.
//
// Exported for `rackctl destroy`, which walks the teardown outside the phase engine.
func DestroyAgentPlatform(ctx context.Context, st *engine.State, opts AgentPlatformTeardown) error {
	clusterEnv, err := agentPlatformEnv(st)
	if err != nil {
		return err
	}
	accountEnv, err := agentPlatformAccountEnv(st)
	if err != nil {
		return err
	}

	prevDir, prevEnv := st.Runner.Dir, st.Runner.Env
	st.Runner.Dir = st.Repos.AgentPlatform
	defer func() { st.Runner.Dir, st.Runner.Env = prevDir, prevEnv }()

	comps := agentPlatformComponents()
	note(st, "agent-platform substrate: destroying in reverse, BEFORE landing-zone — these components "+
		"read agent-iam's and observability's SSM parameters through unguarded data blocks, and a "+
		"destroy PLAN resolves data sources too, so tearing landing-zone down first leaves them "+
		"unable to plan their own teardown")
	noteAccountScopedTeardown(st, comps, opts)

	for i := len(comps) - 1; i >= 0; i-- {
		c := comps[i]
		if c.account && !opts.AccountScoped {
			continue
		}
		vars := clusterEnv
		if c.account {
			vars = accountEnv
			// Act 1 of the two-act bucket teardown, for the account chain specifically.
			// force_destroy has no effect until an apply lands it in state, so a destroy that
			// only passes the variable meets BucketNotEmpty exactly as if it had not.
			if opts.ForceBuckets {
				vars = append(append([]string(nil), vars...), "TF_VAR_force_destroy_buckets=true")
			}
		}
		st.Runner.Env = append(append([]string(nil), prevEnv...), vars...)

		dir := agentPlatformDir(st, c)
		if c.account && opts.ForceBuckets {
			note(st, "agent-platform substrate: %s — landing force_destroy_buckets=true before destroying", c.name)
			if err := st.Runner.Run(ctx, "terragrunt", "--working-dir", dir, "--non-interactive", "init"); err != nil {
				return fmt.Errorf("agent-platform %s: init: %w", c.name, err)
			}
			if err := st.Runner.Run(ctx, "terragrunt", "--working-dir", dir, "--non-interactive",
				"apply", "-auto-approve"); err != nil {
				return fmt.Errorf("agent-platform %s: permitting bucket teardown: %w", c.name, err)
			}
		}

		if err := st.Runner.Run(ctx, "terragrunt", "--working-dir", dir, "--non-interactive", "init"); err != nil {
			return fmt.Errorf("agent-platform %s: init: %w", c.name, err)
		}
		if err := st.Runner.Run(ctx, "terragrunt", "--working-dir", dir, "--non-interactive",
			"destroy", "-auto-approve"); err != nil {
			return fmt.Errorf("agent-platform %s: destroy: %w — if this is BucketNotEmpty, that "+
				"component owns S3 buckets that need force_destroy landed in state first; "+
				"`rackctl destroy --account-scoped --force-buckets` does that two-act sequence", c.name, err)
		}
	}
	return nil
}

// AgentPlatformTeardown says how far a teardown of ONE environment may reach.
//
// The two fields exist because the account roots are not this environment's to delete by
// default, and because reaching them at all is useless without also being allowed to empty what
// they own.
type AgentPlatformTeardown struct {
	// AccountScoped permits destroying live/org/bedrock-account and live/org/cost-pipeline.
	//
	// Off by default, and that default is the whole point. Those two roots hold the account's
	// single Bedrock invocation-logging configuration and its single Cost and Usage Report,
	// shared by every environment installed here. Tearing down development with this on, while
	// production is live, deletes production's invocation logging outright — no name to scope
	// it by, nothing red, and invocation logging is the signal every budget decision reads.
	// That is ledger O14's failure returning through the teardown door rather than the apply
	// door, and an installer does not get to make that call implicitly.
	//
	// Leaving them standing costs a Bedrock logging configuration, a CUR and five buckets in an
	// account with nothing left to use them, which is disclosed by name rather than left to a
	// bill. Deleting production's telemetry costs production.
	AccountScoped bool
	// ForceBuckets lands force_destroy_buckets=true on the account cost-pipeline before
	// destroying it. Only meaningful with AccountScoped.
	//
	// bedrock-account needs no equivalent: its force_destroy is `object_lock_mode !=
	// "COMPLIANCE"`, and live/org/bedrock-account pins GOVERNANCE precisely so the account tears
	// down cleanly. It does quietly require s3:BypassGovernanceRetention on the caller.
	ForceBuckets bool
}

// noteAccountScopedTeardown says exactly what a teardown is about to leave behind, or about to
// take from everyone else.
//
// Naming the survivors matters more here than in the usual disclosure, because target 5 verifies
// a teardown as a set difference against a baseline: anything left standing shows up as an
// unexplained resource unless it was declared in advance.
func noteAccountScopedTeardown(st *engine.State, comps []apComponent, opts AgentPlatformTeardown) {
	var account []string
	for _, c := range comps {
		if c.account {
			account = append(account, c.name)
		}
	}
	if len(account) == 0 {
		return
	}
	if !opts.AccountScoped {
		note(st, "agent-platform substrate: NOT destroying %s — they are account-scoped roots at "+
			"live/org, shared with every other environment in this account. Destroying them from one "+
			"environment's teardown deletes the account's ONLY Bedrock invocation-logging "+
			"configuration and its ONLY cost pipeline, for every environment, silently. They will "+
			"remain: the invocation-logging configuration, the CUR, and the org-<account>-<region>- "+
			"bedrock/cost buckets. If this is the last environment in the account, re-run with "+
			"--account-scoped (add --force-buckets, or the buckets wedge on BucketNotEmpty)",
			strings.Join(account, " and "))
		return
	}
	note(st, "agent-platform substrate: --account-scoped — DESTROYING %s, which are shared with "+
		"every environment in this account. Any other environment still installed here loses its "+
		"Bedrock invocation logging and its budget data source, with nothing going red",
		strings.Join(account, " and "))
}

// CaptureLandingZoneOutputs re-reads the landing-zone outputs the agent-platform substrate
// needs, for the paths that did not apply anything this run.
//
// `rackctl destroy` starts cold: nothing populated State.Outputs, because no component was
// applied. But the values are still THERE — network, secrets and cluster are exactly the
// components a teardown has not reached yet — so reading them back is both possible and
// necessary, since the agent-platform destroy needs the same TF_VARs its apply did.
//
// Best-effort per component, like captureOutputs: a missing one surfaces later as
// needOutput's named error, which says which component should have produced it. That is a
// better failure than aborting a teardown here.
func CaptureLandingZoneOutputs(ctx context.Context, st *engine.State) {
	for _, c := range []string{"network", "secrets", "cluster"} {
		captureOutputs(ctx, st, c)
	}
}
