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

// agentPlatformComponents is the apply order for eks-agent-platform/terraform. Destroy runs
// it in reverse.
//
// Only ONE edge inside this tree is load-bearing, and it is already expressed: every
// cost-pipeline leaf declares `dependency "bedrock"` and wires
// bedrock_invocation_log_group from it. That dependency permits mock outputs for validate,
// plan AND init — but not apply or destroy — so cost-pipeline can init against an unapplied
// bedrock and then fail the apply. It goes last.
//
// The coupling is real infrastructure, not just terragrunt bookkeeping: cost-pipeline creates
// an aws_cloudwatch_log_subscription_filter on the log group bedrock owns. Removing the
// dependency block would not remove the ordering.
//
// batch-runtime is deliberately absent. It is a component with no live root in any
// environment, so there is nothing here to apply — see assertAgentPlatformRoots, which says so
// rather than letting terragrunt discover it.
func agentPlatformComponents() []string {
	return []string{
		"bedrock",
		"agent-egress",
		"accelerator-pools",
		"eval-runtime",
		"kill-switch",
		"cost-pipeline", // last: needs bedrock's invocation log group
	}
}

// agentPlatformDir is the terragrunt path for one component of that tree. The layout is
// live/<environment>-platform/<component> — note the "-platform" suffix on the environment
// directory, which is this tree's own convention and not landing-zone's.
func agentPlatformDir(st *engine.State, component string) string {
	return fmt.Sprintf("terraform/live/%s-platform/%s", st.Config.Environment, component)
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

	// One landing-zone key, two variables. There is no separate "logs" and "data" CMK: the
	// secrets component mints a single key whose policy is the only one in landing-zone
	// granting BOTH logs.<region>.amazonaws.com and bedrock.amazonaws.com — precisely this
	// pair of consumers. landing-zone's own agent-iam wires its data_kms_key_arn from the same
	// output.
	kms, err := needOutput(st, "kms_key_arn", "secrets")
	if err != nil {
		return nil, err
	}
	env = append(env,
		"TF_VAR_logs_kms_key_arn="+kms,
		"TF_VAR_data_kms_key_arn="+kms)

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
	// The names differ across the two trees: landing-zone exports karpenter_node_role_name,
	// this tree's variable is node_role_name. It is unambiguously the Karpenter node role —
	// accelerator-pools attaches an inline policy to it so GPU and Neuron nodes can pull
	// device-plugin images. The name is deterministic (<cluster>-karpenter-node); landing-zone
	// pins it with node_iam_role_use_name_prefix = false precisely so it can be referenced.
	nodeRole, err := needOutput(st, "karpenter_node_role_name", "cluster")
	if err != nil {
		return nil, err
	}
	env = append(env,
		"TF_VAR_cluster_security_group_id="+sg,
		"TF_VAR_node_role_name="+nodeRole)

	return env, nil
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
	env, err := agentPlatformEnv(st)
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
	st.Runner.Env = append(append([]string(nil), prevEnv...), env...)
	defer func() { st.Runner.Dir, st.Runner.Env = prevDir, prevEnv }()

	if st.Runner.DryRun {
		note(st, "agent-platform substrate: (apply) reads kms_key_arn from landing-zone's secrets; "+
			"vpc_id, private_subnet_ids and both route-table lists from network; "+
			"cluster_security_group_id and karpenter_node_role_name from cluster — then hands them "+
			"to this tree as TF_VAR_*. The <placeholders> below are those reads, not values")
	}
	noteAgentPlatformTeardown(st)

	for _, c := range agentPlatformComponents() {
		dir := agentPlatformDir(st, c)
		if err := st.Runner.Run(ctx, "terragrunt", "--working-dir", dir, "--non-interactive", "init"); err != nil {
			return fmt.Errorf("agent-platform %s: init: %w", c, err)
		}
		if err := st.Runner.Run(ctx, "terragrunt", "--working-dir", dir, "--non-interactive",
			"apply", "-auto-approve"); err != nil {
			return fmt.Errorf("agent-platform %s: apply: %w", c, err)
		}
	}
	return nil
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
	note(st, "agent-platform substrate: applying %s", strings.Join(agentPlatformComponents(), ", "))
	note(st, "agent-platform substrate: NOTE — bedrock and cost-pipeline cannot be destroyed cleanly "+
		"once the platform has run. Four S3 buckets between them carry no force-destroy setting "+
		"(bedrock access-logs; cost-pipeline access-logs, cur, athena-results — two of them "+
		"versioned), and all four take writes in normal operation, so a teardown meets "+
		"BucketNotEmpty. Emptying them by hand is the workaround until the components gain the "+
		"force_destroy_buckets input landing-zone's equivalents already have")
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
	var missing []string
	for _, c := range agentPlatformComponents() {
		if _, err := os.Stat(filepath.Join(st.Repos.AgentPlatform, agentPlatformDir(st, c), "terragrunt.hcl")); err != nil {
			missing = append(missing, c)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if st.Runner.DryRun {
		note(st, "PLAN ONLY: this eks-agent-platform checkout has no live root for %s under %s-platform. "+
			"A dry-run does not pull, so the tree on disk predates whatever upstream has now",
			strings.Join(missing, ", "), st.Config.Environment)
		return nil
	}
	return &engine.NoRollbackError{Err: fmt.Errorf(
		"eks-agent-platform has no live root for %s in the %s environment (%s).\n\n"+
			"That tree authors its live roots per environment, and batch-runtime — the one component "+
			"with no root in ANY environment — is excluded here for exactly this reason. If a root has "+
			"gone missing upstream, this is the report; nothing in the agent-platform tree has been "+
			"applied, so nothing needs unwinding",
		strings.Join(missing, ", "), st.Config.Environment,
		agentPlatformDir(st, missing[0]))}
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
func DestroyAgentPlatform(ctx context.Context, st *engine.State) error {
	env, err := agentPlatformEnv(st)
	if err != nil {
		return err
	}

	prevDir, prevEnv := st.Runner.Dir, st.Runner.Env
	st.Runner.Dir = st.Repos.AgentPlatform
	st.Runner.Env = append(append([]string(nil), prevEnv...), env...)
	defer func() { st.Runner.Dir, st.Runner.Env = prevDir, prevEnv }()

	comps := agentPlatformComponents()
	note(st, "agent-platform substrate: destroying in reverse, BEFORE landing-zone — these components "+
		"read agent-iam's and observability's SSM parameters through unguarded data blocks, and a "+
		"destroy PLAN resolves data sources too, so tearing landing-zone down first leaves them "+
		"unable to plan their own teardown")
	for i := len(comps) - 1; i >= 0; i-- {
		c := comps[i]
		dir := agentPlatformDir(st, c)
		if err := st.Runner.Run(ctx, "terragrunt", "--working-dir", dir, "--non-interactive", "init"); err != nil {
			return fmt.Errorf("agent-platform %s: init: %w", c, err)
		}
		if err := st.Runner.Run(ctx, "terragrunt", "--working-dir", dir, "--non-interactive",
			"destroy", "-auto-approve"); err != nil {
			return fmt.Errorf("agent-platform %s: destroy: %w — if this is BucketNotEmpty, that "+
				"component owns S3 buckets with no force-destroy setting and they have to be emptied "+
				"by hand; see the note at apply time", c, err)
		}
	}
	return nil
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
