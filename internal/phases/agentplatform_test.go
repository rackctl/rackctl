package phases

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/engine"
	"github.com/rackctl/rackctl/internal/exec"
)

// apState builds a state with the landing-zone outputs the agent-platform substrate needs,
// exactly as captureOutputs would leave them — lists as compact JSON, per tf.ParseOutputs.
func apState(t *testing.T) (*engine.State, *strings.Builder) {
	t.Helper()
	var out strings.Builder
	run := exec.New(&out)
	run.DryRun = true
	cfg := baseCfg()
	cfg.Environment = config.EnvDev
	cfg.Cluster.Name = "platform"
	cfg.Cloud.AccountID = "111111111111"
	cfg.Cloud.Region = "us-west-2"
	return &engine.State{
		Config: cfg,
		Runner: run,
		Outputs: map[string]string{
			"kms_key_arn":               "arn:aws:kms:us-west-2:111111111111:key/abc",
			"vpc_id":                    "vpc-0abc",
			"private_subnet_ids":        `["subnet-a","subnet-b","subnet-c"]`,
			"private_route_table_ids":   `["rtb-p1","rtb-p2"]`,
			"public_route_table_ids":    `["rtb-x1"]`,
			"cluster_security_group_id": "sg-0abc",
			"karpenter_node_role_name":  "development-platform-karpenter-node",
		},
	}, &out
}

// cluster_name here is the FULL EKS cluster name, the opposite of what rackctl passes
// landing-zone's `cluster` component. Getting it backwards is silent and total: every IAM
// role, log group and SSM path this tree mints would carry "platform" where it should carry
// "development-platform", and nothing would error — the resources would simply be named for a
// cluster that does not exist.
func TestAgentPlatformEnv_SendsTheFullClusterName(t *testing.T) {
	st, _ := apState(t)
	env, err := agentPlatformEnv(st)
	if err != nil {
		t.Fatalf("agentPlatformEnv: %v", err)
	}
	if !slices.Contains(env, "TF_VAR_cluster_name=development-platform") {
		t.Fatalf("this tree takes the composed name, not the base — landing-zone's cluster component "+
			"is the one place that composes it, and everything downstream receives the result.\ngot: %v", env)
	}
	if slices.Contains(env, "TF_VAR_cluster_name=platform") {
		t.Fatal("the BASE name was sent; every resource this tree mints would be misnamed")
	}
}

// AWS_ACCOUNT_ID, not a TF_VAR. env.hcl reads it with get_env() to build the state bucket
// name, which terragrunt resolves at PARSE time — before variables exist — so no TF_VAR can
// reach it. rackctl sets TERRAGRUNT_ACCOUNT_ID for landing-zone, which this tree never reads.
func TestAgentPlatformEnv_SetsTheParseTimeAccountID(t *testing.T) {
	st, _ := apState(t)
	env, err := agentPlatformEnv(st)
	if err != nil {
		t.Fatalf("agentPlatformEnv: %v", err)
	}
	if !slices.Contains(env, "AWS_ACCOUNT_ID=111111111111") {
		t.Fatalf("without AWS_ACCOUNT_ID the state bucket name cannot be built at parse time, and "+
			"TERRAGRUNT_ACCOUNT_ID is a different variable this tree does not read.\ngot: %v", env)
	}
}

// One landing-zone key serves both variables. There is no separate logs/data CMK: the secrets
// component mints a single key whose policy is the only one in landing-zone granting both
// logs.<region>.amazonaws.com and bedrock.amazonaws.com — exactly this pair of consumers.
func TestAgentPlatformEnv_OneCMKServesBothKMSVariables(t *testing.T) {
	st, _ := apState(t)
	env, err := agentPlatformEnv(st)
	if err != nil {
		t.Fatalf("agentPlatformEnv: %v", err)
	}
	arn := "arn:aws:kms:us-west-2:111111111111:key/abc"
	for _, want := range []string{"TF_VAR_logs_kms_key_arn=" + arn, "TF_VAR_data_kms_key_arn=" + arn} {
		if !slices.Contains(env, want) {
			t.Errorf("missing %q — landing-zone's agent-iam wires its own data_kms_key_arn from the "+
				"same secrets output, so a second lookup would be inventing a key that does not exist", want)
		}
	}
}

// route_table_ids has no matching landing-zone output — network exports the two halves
// separately, and agent-egress wants both, because a gateway endpoint installs its
// prefix-list route into every route table it associates with.
func TestAgentPlatformEnv_ConcatenatesBothRouteTableHalves(t *testing.T) {
	st, _ := apState(t)
	env, err := agentPlatformEnv(st)
	if err != nil {
		t.Fatalf("agentPlatformEnv: %v", err)
	}
	if !slices.Contains(env, `TF_VAR_route_table_ids=["rtb-p1","rtb-p2","rtb-x1"]`) {
		t.Fatalf("both private and public route tables must be sent, as one JSON list.\ngot: %v", env)
	}
}

// Public may legitimately be absent — an adopted VPC re-exports none, and a private-only
// cluster has none. Only the private half is required.
func TestAgentPlatformEnv_ToleratesNoPublicRouteTables(t *testing.T) {
	st, _ := apState(t)
	delete(st.Outputs, "public_route_table_ids")
	env, err := agentPlatformEnv(st)
	if err != nil {
		t.Fatalf("a private-only cluster must still resolve route_table_ids: %v", err)
	}
	if !slices.Contains(env, `TF_VAR_route_table_ids=["rtb-p1","rtb-p2"]`) {
		t.Fatalf("expected the private half alone; got %v", env)
	}
}

// The two variables rackctl must NOT set, and this is the sharpest invariant here. An ambient
// TF_VAR beats a leaf's inputs, so injecting a value the leaf already resolves correctly does
// not add safety — it replaces a right answer with a guessed one.
//
// allowed_regions is a policy value pinned identically in all three eval-runtime leaves and
// mirrored by the org SCP. bedrock_invocation_log_group is wired by each cost-pipeline leaf's
// `dependency "bedrock"` block from bedrock's real state.
func TestAgentPlatformEnv_DoesNotOverrideWhatTheLeavesAlreadyResolve(t *testing.T) {
	st, _ := apState(t)
	env, err := agentPlatformEnv(st)
	if err != nil {
		t.Fatalf("agentPlatformEnv: %v", err)
	}
	for _, e := range env {
		for _, forbidden := range []string{"TF_VAR_allowed_regions=", "TF_VAR_bedrock_invocation_log_group="} {
			if strings.HasPrefix(e, forbidden) {
				t.Errorf("rackctl set %q. The leaves resolve this correctly on their own, and an ambient "+
					"TF_VAR silently wins — so this replaces a right value with a guessed one", e)
			}
		}
	}
}

// A missing output must fail loudly and name its producer. captureOutputs is best-effort and
// silent on error, which is right for its advisory uses — but here an empty KMS ARN or VPC id
// does not fail fast, it fails inside a provider call naming neither rackctl nor the component
// that should have produced it.
func TestAgentPlatformEnv_NamesTheProducerOfAMissingOutput(t *testing.T) {
	st, _ := apState(t)
	st.Runner.DryRun = false // the apply path: a dry-run deliberately substitutes a placeholder
	delete(st.Outputs, "kms_key_arn")

	_, err := agentPlatformEnv(st)
	if err == nil {
		t.Fatal("a missing required output must be an error, not an apply with an empty TF_VAR")
	}
	for _, want := range []string{"kms_key_arn", "secrets"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must name %q so the operator knows what did not apply; got: %v", want, err)
		}
	}
}

// cost-pipeline goes last. Its leaf declares `dependency "bedrock"` whose mock outputs cover
// validate, plan AND init — but not apply — so it inits happily against an unapplied bedrock
// and then fails. The coupling is also real infrastructure: cost-pipeline puts a log
// subscription filter on the log group bedrock owns.
func TestAgentPlatformComponents_CostPipelineFollowsBedrock(t *testing.T) {
	comps := agentPlatformComponents()
	b, c := slices.Index(comps, "bedrock"), slices.Index(comps, "cost-pipeline")
	if b < 0 || c < 0 {
		t.Fatalf("both components must be applied; got %v", comps)
	}
	if b > c {
		t.Fatalf("bedrock (%d) must precede cost-pipeline (%d): cost-pipeline resolves "+
			"bedrock_invocation_log_group from bedrock's state and subscribes to the log group it "+
			"owns.\ngot %v", b, c, comps)
	}
}

// batch-runtime has no live root in ANY environment, so applying it would fail on a directory
// with no terragrunt.hcl. It is excluded by name rather than by a directory walk — which would
// also stumble over live/production-platform/agent-iam, a stray holding a lock file and a
// cache but no terragrunt.hcl.
func TestAgentPlatformComponents_ExcludesTheRootlessComponent(t *testing.T) {
	if slices.Contains(agentPlatformComponents(), "batch-runtime") {
		t.Fatal("batch-runtime has no live root in any environment; applying it fails on a directory " +
			"with no terragrunt.hcl")
	}
	if slices.Contains(agentPlatformComponents(), "agent-iam") {
		t.Fatal("live/production-platform/agent-iam is a stray directory with no terragrunt.hcl — " +
			"agent-iam is a LANDING-ZONE component")
	}
}

// The path shape is this tree's own: live/<environment>-platform/<component>. The "-platform"
// suffix is not landing-zone's convention, and getting it wrong points every invocation at a
// directory that does not exist.
func TestAgentPlatformDir_UsesTheEnvironmentPlatformLayout(t *testing.T) {
	st, _ := apState(t)
	if got, want := agentPlatformDir(st, "bedrock"), "terraform/live/development-platform/bedrock"; got != want {
		t.Fatalf("agentPlatformDir = %q, want %q", got, want)
	}
}

// The teardown consequence must be disclosed at apply time, the same way druid's is. bedrock
// and cost-pipeline own four S3 buckets with no force_destroy between them, all written to in
// normal operation, so a destroy meets BucketNotEmpty after any traffic.
func TestAgentPlatform_DisclosesTheTeardownBlocker(t *testing.T) {
	st, out := apState(t)
	noteAgentPlatformTeardown(st)
	if !strings.Contains(out.String(), "cannot be destroyed cleanly") {
		t.Fatalf("applying components that cannot be torn down must say so at apply time — that is "+
			"the rule the druid disclosure established.\ngot:\n%s", out.String())
	}
}

// The state bucket is a SECOND one, and nothing else creates it. landing-zone's
// init-backend-aws.sh hardcodes its own name, and terragrunt will not bootstrap a missing
// backend — so without this every apply in the tree fails after the whole rest of the run has
// succeeded.
func TestAgentPlatform_EnsuresItsOwnStateBucket(t *testing.T) {
	st, out := apState(t)
	if err := ensureAgentPlatformBackend(context.Background(), st); err != nil {
		t.Fatalf("ensureAgentPlatformBackend: %v", err)
	}
	if !strings.Contains(out.String(), "eks-agent-platform-tfstate-111111111111-us-west-2") {
		t.Fatalf("the agent-platform state bucket must be ensured by name; got:\n%s", out.String())
	}
}

// And the env must not leak. These variables belong to one tree; TF_VAR_cluster_name in
// particular carries a DIFFERENT value here than landing-zone's cluster component takes, so a
// leak would rename resources in the repo rackctl spends most of its time in.
func TestApplyAgentPlatform_RestoresTheRunnerEnvAndDir(t *testing.T) {
	st, _ := apState(t)
	st.Repos = engine.Repos{AgentPlatform: t.TempDir()}
	st.Runner.Dir = "/landing-zone"
	st.Runner.Env = []string{"AWS_PROFILE=acme"}

	_ = applyAgentPlatform(context.Background(), st)

	if st.Runner.Dir != "/landing-zone" {
		t.Errorf("Runner.Dir = %q; the phases after this one expect the landing-zone checkout", st.Runner.Dir)
	}
	if len(st.Runner.Env) != 1 || st.Runner.Env[0] != "AWS_PROFILE=acme" {
		t.Errorf("Runner.Env leaked: %v. TF_VAR_cluster_name here is the FULL name, which would "+
			"rename every landing-zone resource that reads it", st.Runner.Env)
	}
}

// A dry-run must PLAN, never fail — and this one nearly shipped failing.
//
// captureOutputs returns immediately without --apply, so on the plan path every landing-zone
// output is absent. needOutput's strict check then turned an ordinary `rackctl init` into a
// phase failure, which CleanOnFail escalated into a full rollback sweep. A plan that cannot
// run is worse than useless: it is the one command an operator runs to find out whether the
// real one will work.
func TestApplyAgentPlatform_DryRunPlansRatherThanFailing(t *testing.T) {
	st, out := apState(t)
	st.Repos = engine.Repos{AgentPlatform: t.TempDir()}
	st.Outputs = map[string]string{} // exactly what a dry-run has: nothing captured

	if err := applyAgentPlatform(context.Background(), st); err != nil {
		t.Fatalf("a dry-run has no captured outputs by construction, so requiring them makes every "+
			"plan fail — and clean-on-failure turns that into a rollback: %v", err)
	}
	// And it must still say what it would read, or the plan hides the thing most likely to go
	// wrong on the real run.
	if !strings.Contains(out.String(), "kms_key_arn") || !strings.Contains(out.String(), "secrets") {
		t.Fatalf("the plan must name the outputs it will read and their producers.\ngot:\n%s", out.String())
	}
}

// But the strictness must survive on the APPLY path, which is where an empty TF_VAR would
// reach a provider and fail with a message naming neither rackctl nor the missing component.
func TestApplyAgentPlatform_ApplyStillRequiresTheOutputs(t *testing.T) {
	st, _ := apState(t)
	st.Runner.DryRun = false
	st.Outputs = map[string]string{}

	if _, err := agentPlatformEnv(st); err == nil {
		t.Fatal("on the apply path a missing output must still be a loud, named error — the " +
			"dry-run placeholder must not have relaxed it")
	}
}

// enable_eval_runtime must be republished onto cluster-bootstrap after the agent-platform
// tree lands, or EvalSuite reports are silently discarded.
//
// The variable is opt-in upstream because it depends on eks-agent-platform's eval-runtime
// component having written its SSM parameters (cluster-bootstrap/variables.tf:182). rackctl
// applies cluster-bootstrap in the gitops phase, one phase BEFORE that tree exists — so the
// flag cannot be set there, and if it is never set at all, bootstrap.tf never stamps
// `eks-agent-platform/eval-reports-bucket` on the ArgoCD cluster Secret. The operator then
// renders an empty bucket and every eval run's reports go nowhere. Nothing errors.
//
// This is the same rule tgenv.go states for enable_managed_monitoring, one flag over.
func TestPlatform_RepublishesClusterBootstrapForEvalRuntime(t *testing.T) {
	// A fake terragrunt on $PATH that records verb, working dir and whether the flag was
	// exported. exec.Runner echoes argv but never env, so a dry-run transcript cannot tell
	// the flag apart from the note that mentions it — asserting on the transcript is how a
	// test like this passes while the behaviour is gone.
	dir := t.TempDir()
	logf := filepath.Join(dir, "invocations")
	script := fmt.Sprintf(`#!/bin/sh
for a in "$@"; do
  case "$a" in apply|destroy|init) verb="$a";; esac
  case "$a" in */cluster-bootstrap) comp="cluster-bootstrap";; esac
done
[ -n "$comp" ] && echo "$verb $comp eval_runtime=${TF_VAR_enable_eval_runtime:-UNSET}" >> %q
exit 0
`, logf)
	if err := os.WriteFile(filepath.Join(dir, "terragrunt"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "aws"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// The live roots the phase asserts before applying anything.
	for _, c := range agentPlatformComponents() {
		root := filepath.Join(dir, "terraform/live/development-platform", c)
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "terragrunt.hcl"), []byte("# fixture\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "live/aws/workload-development/us-west-2/development/cluster-bootstrap"), 0o755); err != nil {
		t.Fatal(err)
	}

	st, _ := apState(t)
	st.Runner = exec.New(io.Discard) // NOT dry-run — the point is to exec
	st.Repos = engine.Repos{Workdir: dir, LandingZone: dir, AgentPlatform: dir}

	if err := (platform{}).Run(context.Background(), st); err != nil {
		t.Fatalf("platform run: %v", err)
	}

	b, err := os.ReadFile(logf)
	if err != nil {
		t.Fatalf("cluster-bootstrap was never re-applied. eval-runtime's SSM parameters exist only "+
			"after the agent-platform tree lands, so this republish is the ONLY point at which "+
			"eks-agent-platform/eval-reports-bucket can be stamped on the ArgoCD cluster Secret. "+
			"Without it the operator renders an empty bucket and every EvalSuite report is "+
			"discarded silently: %v", err)
	}
	got := string(b)
	if !strings.Contains(got, "apply cluster-bootstrap eval_runtime=true") {
		t.Fatalf("the republish must carry TF_VAR_enable_eval_runtime=true to the terragrunt "+
			"process:\n%s", got)
	}
	// The flag must not leak: cluster-bootstrap is the only component that declares it.
	if len(st.Runner.Env) != 0 {
		t.Fatalf("Runner.Env must be restored after the republish, got %v", st.Runner.Env)
	}
}
