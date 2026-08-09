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
			// The two CMK outputs are deliberately DIFFERENT here. They are equal in every
			// default deployment — secrets publishes one key under both names unless an
			// environment sets separate_logs_key — and a fixture that reproduced that equality
			// would make the wiring untestable: crossing the two variables, or dropping back to
			// one lookup, would still pass.
			"kms_key_arn":               "arn:aws:kms:us-west-2:111111111111:key/data",
			"logs_kms_key_arn":          "arn:aws:kms:us-west-2:111111111111:key/logs",
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

// Each KMS variable follows its own landing-zone output.
//
// This test previously asserted the opposite — that one output served both — and explained that
// a second lookup "would be inventing a key that does not exist". That was correct until
// landing-zone#205, which added `separate_logs_key` and a second output. The output exists now,
// and reading it is what keeps the wiring correct once an environment separates: on separation
// the log-path grants MOVE off the secrets key rather than being copied, so an installer still
// passing the data key here fails at CreateLogGroup.
func TestAgentPlatformEnv_EachKMSVariableFollowsItsOwnOutput(t *testing.T) {
	st, _ := apState(t)
	env, err := agentPlatformEnv(st)
	if err != nil {
		t.Fatalf("agentPlatformEnv: %v", err)
	}
	for _, want := range []string{
		"TF_VAR_logs_kms_key_arn=arn:aws:kms:us-west-2:111111111111:key/logs",
		"TF_VAR_data_kms_key_arn=arn:aws:kms:us-west-2:111111111111:key/data",
	} {
		if !slices.Contains(env, want) {
			t.Errorf("missing %q — each variable reads the output published for it. "+
				"logs_kms_key_arn is published in BOTH modes and equals kms_key_arn when sharing, "+
				"so reading it is correct whether or not this environment separates.\ngot: %v", want, env)
		}
	}
	// And specifically not crossed, which a Contains-only assertion would miss if both
	// variables were somehow set twice.
	if slices.Contains(env, "TF_VAR_logs_kms_key_arn=arn:aws:kms:us-west-2:111111111111:key/data") {
		t.Error("the DATA key was sent as the log-path key. Once an environment separates, the " +
			"secrets key no longer admits logs.<region>.amazonaws.com and every CreateLogGroup fails")
	}
}

// A secrets state written before landing-zone#205 publishes no logs_kms_key_arn, and that must
// not be a hard failure — most sharply on the teardown path, which reads these outputs back out
// of a state nothing has re-applied.
//
// The fallback is sound rather than convenient: the output is published in BOTH modes, so a
// state missing it came from a module that could not separate the keys, which means the one key
// it did publish is the log key too.
func TestAgentPlatformEnv_FallsBackWhenTheSecretsStatePredatesTheLogsOutput(t *testing.T) {
	st, out := apState(t)
	st.Runner.DryRun = false // the apply/destroy path; a dry-run substitutes placeholders
	delete(st.Outputs, "logs_kms_key_arn")

	env, err := agentPlatformEnv(st)
	if err != nil {
		t.Fatalf("an older secrets state must still resolve — hard-failing here would refuse to "+
			"tear down every platform installed before landing-zone#205: %v", err)
	}
	want := "TF_VAR_logs_kms_key_arn=arn:aws:kms:us-west-2:111111111111:key/data"
	if !slices.Contains(env, want) {
		t.Fatalf("expected the secrets key to stand in for the log path; got %v", env)
	}
	if !strings.Contains(out.String(), "predates") {
		t.Errorf("the fallback must say why it happened — a silent one is indistinguishable from "+
			"reading the right output.\ngot:\n%s", out.String())
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

// apIndex is the position of a component in the apply order, or -1.
func apIndex(comps []apComponent, name string) int {
	return slices.IndexFunc(comps, func(c apComponent) bool { return c.name == name })
}

// The apply order is two chains, and every edge in it is real.
//
// bedrock-account first: it is the only root with no upstream dependency, and the account cost
// pipeline reads its invocation log-group name out of SSM. That edge USED to be a terragrunt
// `dependency` block, which meant terragrunt would enforce it; it is an SSM read now, which
// means nothing enforces it but this slice. Getting it backwards fails at apply against a
// parameter that does not exist yet.
//
// cost-access last: it joins the account contract (/eks-agent-platform/org/cost-pipeline/*) to
// this cluster's operator, and republishes the account values under the cluster key.
func TestAgentPlatformComponents_OrderingHoldsBothChains(t *testing.T) {
	comps := allAPComponents()
	for _, edge := range []struct{ first, then, why string }{
		{"bedrock-account", "cost-pipeline",
			"the account cost pipeline reads bedrock-account's invocation log group from SSM and " +
				"subscribes to the log group it owns"},
		{"cost-pipeline", "cost-access",
			"cost-access reads the account contract cost-pipeline publishes under " +
				"/eks-agent-platform/org/cost-pipeline/* and republishes it under this cluster's key"},
	} {
		a, b := apIndex(comps, edge.first), apIndex(comps, edge.then)
		if a < 0 || b < 0 {
			t.Fatalf("both %q and %q must be applied; got %v", edge.first, edge.then,
				agentPlatformComponentNames(comps))
		}
		if a > b {
			t.Errorf("%s (%d) must precede %s (%d): %s", edge.first, a, edge.then, b, edge.why)
		}
	}
	// cost-access is the join and belongs at the end of the cluster chain.
	if got := apIndex(comps, "cost-access"); got != len(comps)-1 {
		t.Errorf("cost-access is at %d of %d; it reads landing-zone's agent-iam contract AND the "+
			"account contract, so it is the last thing that can succeed", got, len(comps)-1)
	}
}

// The components that no longer exist must be GONE, not merely joined by their replacements.
//
// cost-pipeline moved to live/org and cost-access took its place per cluster. A list still
// naming a per-cluster cost-pipeline makes assertAgentPlatformRoots collect it as missing, and a
// non-dry run then returns NoRollbackError — the phase aborts before applying anything, which is
// exactly what shipped.
func TestAgentPlatformComponents_MatchesTheTreeOnDisk(t *testing.T) {
	comps := allAPComponents()
	names := agentPlatformComponentNames(comps)

	for _, want := range []string{"bedrock-account", "cost-pipeline", "cost-access"} {
		if apIndex(comps, want) < 0 {
			t.Errorf("missing %q — the tree has it; not applying it leaves the platform without "+
				"part of its substrate", want)
		}
	}
	// batch-runtime has no live root in ANY environment, so applying it would fail on a
	// directory with no terragrunt.hcl. Excluded by name rather than by a directory walk —
	// which would also stumble over live/production-platform/agent-iam, a stray holding a lock
	// file and a cache but no terragrunt.hcl.
	for _, gone := range []string{"batch-runtime", "agent-iam"} {
		if apIndex(comps, gone) >= 0 {
			t.Errorf("%q has no live root to apply; %v", gone, names)
		}
	}
	// And the cardinalities must be right, since the path template follows from them.
	for _, c := range comps {
		wantAccount := c.name == "bedrock-account" || c.name == "cost-pipeline"
		if c.account != wantAccount {
			t.Errorf("%q: account=%v, want %v — cardinality decides the path, the variables it is "+
				"handed, and whether one environment's teardown may remove it",
				c.name, c.account, wantAccount)
		}
	}
}

// Two path shapes, and a single template cannot express both. Per cluster it is
// live/<environment>-platform/<component> — the "-platform" suffix is this tree's own convention
// and not landing-zone's. Account roots are at live/org/<component>, with no environment token
// at all, because there is exactly one of the thing they name.
func TestAgentPlatformDir_AddressesBothCardinalities(t *testing.T) {
	st, _ := apState(t)
	for _, tc := range []struct {
		c    apComponent
		want string
	}{
		{apComponent{name: "bedrock"}, "terraform/live/development-platform/bedrock"},
		{apComponent{name: "cost-access"}, "terraform/live/development-platform/cost-access"},
		{apComponent{name: "bedrock-account", account: true}, "terraform/live/org/bedrock-account"},
		{apComponent{name: "cost-pipeline", account: true}, "terraform/live/org/cost-pipeline"},
	} {
		if got := agentPlatformDir(st, tc.c); got != tc.want {
			t.Errorf("agentPlatformDir(%q, account=%v) = %q, want %q", tc.c.name, tc.c.account, got, tc.want)
		}
	}
	// The account path must carry no environment token. Composing it from the config would put
	// `development` where the tree pins `org`, and live/org/env.hcl validates that literal.
	if got := agentPlatformDir(st, apComponent{name: "cost-pipeline", account: true}); strings.Contains(got, "development") {
		t.Errorf("the account root path carries an environment token: %q", got)
	}
}

// The teardown consequence must be disclosed at apply time, the same way druid's is. The
// account-scoped buckets take writes from the first PUT, so a destroy meets BucketNotEmpty
// unless force_destroy is landed in state first.
func TestAgentPlatform_DisclosesTheTeardownBlocker(t *testing.T) {
	st, out := apState(t)
	noteAgentPlatformTeardown(st)
	if !strings.Contains(out.String(), "BucketNotEmpty") || !strings.Contains(out.String(), "force_destroy") {
		t.Fatalf("applying components that need a two-act teardown must say so at apply time — that "+
			"is the rule the druid disclosure established.\ngot:\n%s", out.String())
	}
}

// Applying an account-scoped root from ONE environment is a fact about the whole account, and it
// has to be said where it is read rather than discovered from a bill or an outage.
func TestAgentPlatform_DisclosesTheAccountScopedApply(t *testing.T) {
	st, out := apState(t)
	noteAccountScopedApply(st)
	got := out.String()
	if !strings.Contains(got, "live/org") || !strings.Contains(got, "ONCE") {
		t.Errorf("the apply must say these roots are account-scoped and shared:\n%s", got)
	}
	// The CMK hazard specifically: landing-zone mints one secrets key per ENVIRONMENT, so
	// whichever environment installs first hands the account its key and a later one repoints
	// it. Nothing in rackctl can fix that from here, which is exactly why it must be stated.
	if !strings.Contains(got, "CMK") || !strings.Contains(got, "per environment") {
		t.Errorf("the apply must disclose that this environment's CMK becomes the ACCOUNT's:\n%s", got)
	}
}

// A one-environment teardown must NOT remove the account-scoped roots, and must name what it is
// leaving — target 5 verifies teardown as a set difference against a baseline, so an undeclared
// survivor reads as an unexplained resource.
func TestAgentPlatformTeardown_LeavesTheAccountRootsAndSaysSo(t *testing.T) {
	st, out := apState(t)
	noteAccountScopedTeardown(st, allAPComponents(), AgentPlatformTeardown{})
	got := out.String()
	if !strings.Contains(got, "NOT destroying") {
		t.Fatalf("the default teardown must state that it is leaving the account roots:\n%s", got)
	}
	for _, want := range []string{"bedrock-account", "cost-pipeline", "--account-scoped"} {
		if !strings.Contains(got, want) {
			t.Errorf("the note must name %q — what survives has to be listed, and the way to remove "+
				"it has to be reachable from the message:\n%s", want, got)
		}
	}
}

// And opting in must say what it costs, because the operator is now doing the thing O14 exists
// to prevent — deliberately, which is fine, and silently, which is not.
func TestAgentPlatformTeardown_OptingInWarnsItIsAccountWide(t *testing.T) {
	st, out := apState(t)
	noteAccountScopedTeardown(st, allAPComponents(), AgentPlatformTeardown{AccountScoped: true})
	got := out.String()
	if !strings.Contains(got, "DESTROYING") || !strings.Contains(got, "every environment") {
		t.Fatalf("--account-scoped must say the blast radius is the account, not this environment:\n%s", got)
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
// output is absent. needOutput's strict check then turned an ordinary `rackctl apply` into a
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

	// The live roots the phase asserts before applying anything. Built through agentPlatformDir
	// rather than by hand, so the fixture cannot quietly disagree with the path the phase uses —
	// which is precisely how a per-cluster cost-pipeline stayed in the list after the tree moved
	// it to live/org.
	fixtureSt, _ := apState(t)
	for _, c := range allAPComponents() {
		root := filepath.Join(dir, agentPlatformDir(fixtureSt, c))
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

// apDestroyLog runs DestroyAgentPlatform against a fake terragrunt on $PATH and returns one
// line per invocation: "<verb> <working-dir> force=<TF_VAR_force_destroy_buckets>".
//
// It execs rather than dry-runs deliberately. The two tests above this one assert on the NOTE a
// teardown prints, and a note is not the behaviour — an adversarial review of this change found
// that deleting the account-root guard entirely left the whole suite green, because the note is
// emitted before the loop and does not change. exec.Runner echoes argv but never env, so the
// force_destroy flag in particular cannot be observed any other way.
func apDestroyLog(t *testing.T, opts AgentPlatformTeardown) []string {
	t.Helper()
	dir := t.TempDir()
	logf := filepath.Join(dir, "invocations")
	script := fmt.Sprintf(`#!/bin/sh
for a in "$@"; do
  case "$prev" in --working-dir) wd="$a";; esac
  case "$a" in apply|destroy|init) verb="$a";; esac
  prev="$a"
done
[ "$verb" != init ] && echo "$verb $wd force=${TF_VAR_force_destroy_buckets:-UNSET}" >> %q
exit 0
`, logf)
	if err := os.WriteFile(filepath.Join(dir, "terragrunt"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// The teardown head-buckets this tree's state bucket first and skips everything when
	// it is absent, because absent means the tree was never applied. These fixtures are
	// all "it WAS applied", so the fake aws answers yes.
	if err := os.WriteFile(filepath.Join(dir, "aws"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	st, _ := apState(t)
	st.Runner = exec.New(io.Discard) // NOT dry-run — the point is to exec
	st.Repos = engine.Repos{AgentPlatform: dir}

	if err := DestroyAgentPlatform(context.Background(), st, opts); err != nil {
		t.Fatalf("DestroyAgentPlatform: %v", err)
	}
	b, err := os.ReadFile(logf)
	if err != nil {
		t.Fatalf("the teardown ran nothing at all: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// The default teardown must not TOUCH the account roots, and this asserts the loop rather than
// the message.
//
// A rollback calls this path automatically with a zero-valued AgentPlatformTeardown, so a guard
// that fails open here means a failed development apply deletes the account's only Bedrock
// invocation-logging configuration and its only cost pipeline — production's included — with
// nothing red.
func TestDestroyAgentPlatform_DefaultNeverReachesTheAccountRoots(t *testing.T) {
	got := apDestroyLog(t, AgentPlatformTeardown{})

	for _, line := range got {
		if strings.Contains(line, "live/org/") {
			t.Fatalf("a single environment's teardown invoked terragrunt against an account root: %q\n"+
				"Those roots hold the account's ONLY Bedrock invocation-logging configuration and its "+
				"ONLY cost pipeline, shared by every environment installed here.\nall: %v", line, got)
		}
	}
	// And it must still have destroyed every cluster root, or the guard is over-broad. The
	// expected count is derived rather than written down: this assertion is about the GUARD, and
	// pinning a literal here would make it fail every time upstream adds or removes a component
	// — which is a different fact, owned by TestAgentPlatformComponents_MatchesTheTreeOnDisk.
	wantCluster := 0
	for _, c := range allAPComponents() {
		if !c.account {
			wantCluster++
		}
	}
	if len(got) != wantCluster {
		t.Fatalf("expected the %d cluster roots to be destroyed; got %d:\n%v", wantCluster, len(got), got)
	}
	// Reverse of the apply order: cost-access first, bedrock last.
	if !strings.Contains(got[0], "cost-access") {
		t.Errorf("teardown must run in reverse, so the join goes first; got %q", got[0])
	}
	if !strings.Contains(got[len(got)-1], "bedrock") {
		t.Errorf("teardown must run in reverse, so bedrock goes last; got %q", got[len(got)-1])
	}
}

// Opting in must actually reach them — a guard that never opens is a flag that lies.
func TestDestroyAgentPlatform_AccountScopedReachesBothAccountRoots(t *testing.T) {
	got := apDestroyLog(t, AgentPlatformTeardown{AccountScoped: true})

	for _, want := range []string{"live/org/cost-pipeline", "live/org/bedrock-account"} {
		found := false
		for _, line := range got {
			if strings.HasPrefix(line, "destroy ") && strings.Contains(line, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("--account-scoped did not destroy %s:\n%v", want, got)
		}
	}
	// Still in reverse: bedrock-account was applied first, so it comes down last.
	if last := got[len(got)-1]; !strings.Contains(last, "live/org/bedrock-account") {
		t.Errorf("bedrock-account is the head of the account chain and must come down last; got %q", last)
	}
}

// The two-act sequence must be TWO acts, in order, on the root that needs it.
//
// force_destroy has no effect until an apply lands it in state — upstream says so in
// cost-pipeline/variables.tf — so a destroy that merely passes the variable meets BucketNotEmpty
// exactly as if it had not been passed. Both halves are asserted because both survived mutation:
// disabling the pre-apply, and dropping the TF_VAR, each left the suite green.
func TestDestroyAgentPlatform_ForceBucketsIsTwoActsAndCarriesTheFlag(t *testing.T) {
	got := apDestroyLog(t, AgentPlatformTeardown{AccountScoped: true, ForceBuckets: true})

	applyAt, destroyAt := -1, -1
	for i, line := range got {
		if !strings.Contains(line, "live/org/cost-pipeline") {
			continue
		}
		switch {
		case strings.HasPrefix(line, "apply ") && applyAt < 0:
			applyAt = i
			if !strings.Contains(line, "force=true") {
				t.Errorf("act 1 ran without TF_VAR_force_destroy_buckets=true, so it lands nothing in "+
					"state and act 2 wedges on BucketNotEmpty anyway: %q", line)
			}
		case strings.HasPrefix(line, "destroy "):
			destroyAt = i
		}
	}
	if applyAt < 0 {
		t.Fatalf("no permitting apply ran for the account cost-pipeline. force_destroy has no effect "+
			"until an apply lands it in state, so --force-buckets would change nothing:\n%v", got)
	}
	if destroyAt < 0 {
		t.Fatalf("the account cost-pipeline was never destroyed:\n%v", got)
	}
	if applyAt > destroyAt {
		t.Fatalf("the permitting apply ran AFTER the destroy (%d > %d) — the two acts are in the "+
			"wrong order and the flag accomplishes nothing:\n%v", applyAt, destroyAt, got)
	}

	// bedrock-account needs no permitting apply: its force_destroy derives from
	// object_lock_mode != COMPLIANCE, and live/org pins GOVERNANCE. An apply there would be a
	// second write to the account's invocation-logging configuration for no reason.
	for _, line := range got {
		if strings.HasPrefix(line, "apply ") && strings.Contains(line, "bedrock-account") {
			t.Errorf("bedrock-account got a permitting apply it does not need: %q", line)
		}
	}

	// And the cluster roots must not carry the flag — they are landing-zone's two-act sequence,
	// run separately by PermitBucketTeardown, and this tree's components do not all declare it.
	for _, line := range got {
		if !strings.Contains(line, "live/org/") && strings.Contains(line, "force=true") {
			t.Errorf("a cluster root carried TF_VAR_force_destroy_buckets: %q", line)
		}
	}
}

// The component list must be the EXACT set, asserted as a literal.
//
// TestAgentPlatformComponents_MatchesTheTreeOnDisk checks that certain names are present and
// certain others absent, which lets a name nobody intended slip in between the two lists —
// verified: adding accelerator-pools back passed the entire suite. It could not do otherwise,
// because every fixture in this file builds its roots by walking allAPComponents()
// through agentPlatformDir(), so the fixture and the list can never disagree.
//
// A literal set does not prove the tree has these roots. What it does is make adding or removing
// one require editing a second place, which is the prompt to go and look — and upstream has
// deleted a root out from under this list twice in two days.
func TestAgentPlatformComponents_IsTheExactSet(t *testing.T) {
	want := []string{
		"bedrock-account", "cost-pipeline", // account, live/org
		"bedrock", "agent-egress", "eval-runtime", "kill-switch", "cost-access", // per cluster
	}
	got := agentPlatformComponentNames(allAPComponents())
	if !slices.Equal(got, want) {
		t.Fatalf("the apply order changed.\n got: %v\nwant: %v\n\n"+
			"If upstream moved, added or deleted a component, update BOTH this literal and the "+
			"slice — and check terraform/live in the eks-agent-platform checkout while you are "+
			"there, because a name here with no root there aborts the phase before it applies "+
			"anything, and a dry run still looks fine.", got, want)
	}
}

// And where a real checkout is available, every root must actually resolve.
//
// This is the only assertion in the file that can catch upstream DELETING a root, which is a
// change to somebody else's repo that no rackctl commit accompanies. It has happened twice:
// cost-pipeline moving to live/org (ledger O21), and accelerator-pools being deleted outright
// (O27). Both times the first symptom was the phase aborting before applying anything, and both
// times `rackctl plan` still looked fine, because a dry run only prints a note.
//
// Skips when no checkout is present rather than failing, since most machines running these tests
// have never run an install. RACKCTL_AGENT_PLATFORM_CHECKOUT points it at a working clone.
func TestAgentPlatformComponents_EveryRootResolvesInARealCheckout(t *testing.T) {
	root := os.Getenv("RACKCTL_AGENT_PLATFORM_CHECKOUT")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skip("no home directory to look for a checkout in")
		}
		// rackctl clones to ~/.rackctl/<org>/eks-agent-platform; the org is not knowable here.
		matches, _ := filepath.Glob(filepath.Join(home, ".rackctl", "*", "eks-agent-platform"))
		for _, m := range matches {
			if _, err := os.Stat(filepath.Join(m, "terraform", "live")); err == nil {
				root = m
				break
			}
		}
	}
	if root == "" {
		t.Skip("no eks-agent-platform checkout found — set RACKCTL_AGENT_PLATFORM_CHECKOUT to run this")
	}

	st, _ := apState(t)
	// Check against the environments the checkout actually authors, so this does not fail on a
	// tree that simply has not grown a staging leaf yet.
	var envs []config.Environment
	for _, e := range []config.Environment{config.EnvDev, config.EnvStaging, config.EnvProduction} {
		if _, err := os.Stat(filepath.Join(root, "terraform", "live", string(e)+"-platform")); err == nil {
			envs = append(envs, e)
		}
	}
	if len(envs) == 0 {
		t.Skipf("%s has no <environment>-platform roots; not a tree this test can check", root)
	}

	for _, env := range envs {
		st.Config.Environment = env
		for _, c := range allAPComponents() {
			dir := agentPlatformDir(st, c)
			if _, err := os.Stat(filepath.Join(root, dir, "terragrunt.hcl")); err != nil {
				t.Errorf("%s/terragrunt.hcl does not exist in %s.\n\n"+
					"rackctl lists %q, so assertAgentPlatformRoots collects it as missing and a "+
					"non-dry run returns NoRollbackError — the phase aborts before applying "+
					"ANYTHING. A dry run only prints a note, so `rackctl plan` would still look fine.",
					dir, root, c.name)
			}
		}
	}
}

// allAPComponents is the full seven — every fixture in these files predates the
// agentPlatform.costPipeline gate and asserts against the complete list, which is what an
// omitted field still produces.
//
// A helper rather than a literal on purpose: a fixture carrying its own copy of the slice
// would keep passing after a component was added to the real one, and "the tests pass
// because they stopped looking at the thing under test" is the failure this repo's gates
// exist to avoid.
func allAPComponents() []apComponent { return agentPlatformComponents(&config.Config{}) }

// The gate removes BOTH cost roots or neither.
//
// Half the pair is worse than either whole answer. cost-access exists only to read what
// cost-pipeline publishes under /eks-agent-platform/org/cost-pipeline/* and republish it
// per cluster, so applied alone it resolves a contract with no producer and fails on eight
// unguarded reads; cost-pipeline alone is the mirror image, an account pipeline no cluster
// can reach. So this asserts the pairing, not just the count.
func TestAgentPlatformComponents_CostRootsAreGatedAsAPair(t *testing.T) {
	off := false
	cfg := &config.Config{}
	cfg.AgentPlatform.CostPipeline = &off

	got := agentPlatformComponentNames(agentPlatformComponents(cfg))
	for _, banned := range []string{"cost-pipeline", "cost-access"} {
		if slicesContainsFunc(got, func(c string) bool { return c == banned }) {
			t.Errorf("costPipeline: false must drop %s.\ngot: %v", banned, got)
		}
	}
	// The rest of the tree is untouched — turning the cost tier off is not turning the
	// agent platform off.
	for _, want := range []string{"bedrock-account", "bedrock", "agent-egress", "eval-runtime", "kill-switch"} {
		if !slicesContainsFunc(got, func(c string) bool { return c == want }) {
			t.Errorf("costPipeline: false must not drop %s.\ngot: %v", want, got)
		}
	}

	// Default (field omitted) keeps both, in their load-bearing positions: cost-pipeline
	// straight after bedrock-account whose log group it subscribes to, cost-access last
	// because it is the join.
	all := agentPlatformComponentNames(allAPComponents())
	if len(all) != 7 || all[1] != "cost-pipeline" || all[len(all)-1] != "cost-access" {
		t.Fatalf("an omitted costPipeline must keep the full ordered seven.\ngot: %v", all)
	}
}

// A tree that was never applied must not wedge the teardown.
//
// This tree writes to its own state bucket, created by the platform phase and by nothing
// else. `terragrunt init` against a missing bucket errors, and `rackctl destroy` used to
// return that error before it reached the landing-zone components — so a run that failed
// in the cluster or gitops phase left the EKS control plane, the VPC and the NAT gateway
// billing, and reported a component tree that had never been applied. rackctl points
// operators at `rackctl destroy` from three separate pre-platform failure branches, so
// this is a designed outcome rather than an edge case.
func TestDestroyAgentPlatform_SkipsWhenTheTreeWasNeverApplied(t *testing.T) {
	dir := t.TempDir()
	logf := filepath.Join(dir, "invocations")
	// terragrunt records any invocation; aws fails head-bucket, i.e. no state bucket.
	if err := os.WriteFile(filepath.Join(dir, "terragrunt"),
		[]byte(fmt.Sprintf("#!/bin/sh\necho called >> %q\nexit 0\n", logf)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "aws"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	st, _ := apState(t)
	st.Runner = exec.New(io.Discard)
	st.Repos = engine.Repos{AgentPlatform: dir}

	if err := DestroyAgentPlatform(context.Background(), st, AgentPlatformTeardown{}); err != nil {
		t.Fatalf("a tree that was never applied is nothing to destroy, not an error: %v", err)
	}
	if _, err := os.Stat(logf); err == nil {
		t.Fatal("terragrunt was invoked against a tree with no state bucket — that init fails, " +
			"and its error is what used to abort the whole teardown before the EKS cluster")
	}
}
