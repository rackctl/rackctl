package phases

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/engine"
	"github.com/rackctl/rackctl/internal/exec"
)

// apDestroyFakes puts a recording terragrunt and an aws answering from awsBody on PATH, and
// returns a non-dry-run State whose landing-zone outputs are those apState carries.
func apDestroyFakes(t *testing.T, awsBody string) (*engine.State, string) {
	t.Helper()
	dir := t.TempDir()
	logf := filepath.Join(dir, "invocations")
	if err := os.WriteFile(filepath.Join(dir, "terragrunt"),
		[]byte(fmt.Sprintf("#!/bin/sh\necho \"$*\" >> %q\nexit 0\n", logf)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "aws"), []byte("#!/bin/sh\n"+awsBody), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	st, _ := apState(t)
	st.Runner = exec.New(io.Discard)
	st.Runner.ReadAttempts = 1
	st.Repos = engine.Repos{AgentPlatform: dir}
	return st, logf
}

func terragruntRan(logf string) bool {
	_, err := os.Stat(logf)
	return err == nil
}

// A 403 on the state bucket is a bucket this run could not see, not a tree that was never
// applied. Reading it as absence skipped a tree that may still hold IAM, SSM and buckets.
func TestDestroyAgentPlatform_ForbiddenStateBucketIsNotAbsence(t *testing.T) {
	st, logf := apDestroyFakes(t, `echo 'An error occurred (403) when calling the HeadBucket operation: Forbidden' >&2; exit 254`)

	err := DestroyAgentPlatform(context.Background(), st, AgentPlatformTeardown{})
	if err == nil {
		t.Fatal("a forbidden head-bucket was read as a tree that was never applied")
	}
	if !strings.Contains(err.Error(), "Forbidden") || !strings.Contains(err.Error(), "eks-agent-platform-tfstate-") {
		t.Errorf("the error must name the bucket and the reason: %v", err)
	}
	if terragruntRan(logf) {
		t.Error("terragrunt ran against a tree whose state could not be established")
	}
}

// The head-bucket is a read and runs in dry-run, so a dry-run shows the same decision the
// teardown would make rather than assuming the tree exists.
func TestDestroyAgentPlatform_DryRunAsksWhetherTheTreeExists(t *testing.T) {
	st, _ := apDestroyFakes(t, `echo 'An error occurred (404) when calling the HeadBucket operation: Not Found' >&2; exit 254`)
	var out strings.Builder
	st.Runner = exec.New(&out)
	st.Runner.DryRun = true

	if err := DestroyAgentPlatform(context.Background(), st, AgentPlatformTeardown{}); err != nil {
		t.Fatalf("an absent bucket is nothing to destroy: %v", err)
	}
	if !strings.Contains(out.String(), "does not exist") {
		t.Errorf("the dry-run must report what the read found:\n%s", out.String())
	}
}

// The bucket outlives the tree, so on a second teardown the outputs are gone. That is
// "nothing to unwind" only when no component still holds state — a first teardown whose
// agent-platform destroy FAILED leaves exactly this shape with the resources still standing.
func TestDestroyAgentPlatform_MissingOutputsWithStateStandingIsAnError(t *testing.T) {
	st, logf := apDestroyFakes(t, `
case "$1 $2" in
  "s3api head-bucket")    exit 0 ;;
  "s3api list-objects-v2") printf 'development-platform/bedrock/terraform.tfstate\tstaging-platform/kill-switch/terraform.tfstate\tdevelopment-platform/kill-switch/terraform.tfstate\n' ;;
  "s3 cp") case "$3" in
             */development-platform/bedrock/*) echo '{"resources":[{"type":"aws_iam_role"},{"type":"aws_kms_key"}]}' ;;
             *) echo '{"resources":[]}' ;;
           esac ;;
  *) exit 1 ;;
esac`)
	delete(st.Outputs, "kms_key_arn")
	st.CaptureErr = map[string]error{"secrets": errors.New("terragrunt: exit status 1: no state")}

	err := DestroyAgentPlatform(context.Background(), st, AgentPlatformTeardown{})
	if err == nil {
		t.Fatal("a tree whose bedrock root still holds two resources was reported as nothing to unwind")
	}
	for _, want := range []string{"bedrock (2 resource(s))", "reading secrets's outputs failed", "no state"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must carry %q: %v", want, err)
		}
	}
	// Another environment's state shares the bucket and must not be reported as this one's.
	if strings.Contains(err.Error(), "kill-switch") {
		t.Errorf("a destroyed root, or another environment's, was counted: %v", err)
	}
	if terragruntRan(logf) {
		t.Error("terragrunt ran without the outputs its destroy plan needs")
	}
}

// With no state standing, missing outputs really are nothing to unwind.
func TestDestroyAgentPlatform_MissingOutputsWithNoStateIsNothingToUnwind(t *testing.T) {
	st, _ := apDestroyFakes(t, `
case "$1 $2" in
  "s3api head-bucket")    exit 0 ;;
  "s3api list-objects-v2") printf 'development-platform/bedrock/terraform.tfstate\torg/bedrock-account/terraform.tfstate\n' ;;
  "s3 cp") case "$3" in
             */org/*) echo '{"resources":[{"type":"aws_s3_bucket"}]}' ;;
             *) echo '{"resources":[]}' ;;
           esac ;;
  *) exit 1 ;;
esac`)
	delete(st.Outputs, "kms_key_arn")

	// The account root holds state, and without --account-scoped it is left standing on
	// purpose — so it is not this teardown's to report.
	if err := DestroyAgentPlatform(context.Background(), st, AgentPlatformTeardown{}); err != nil {
		t.Fatalf("emptied roots and an account root this teardown does not reach are nothing to unwind: %v", err)
	}
	// With --account-scoped, the same account root IS this teardown's.
	err := DestroyAgentPlatform(context.Background(), st, AgentPlatformTeardown{AccountScoped: true})
	if err == nil || !strings.Contains(err.Error(), "bedrock-account") {
		t.Fatalf("--account-scoped reaches the account root, so its standing state must be reported: %v", err)
	}
}

// When the state itself cannot be read, the teardown cannot claim the tree is gone.
func TestDestroyAgentPlatform_UnreadableStateIsAnError(t *testing.T) {
	for name, body := range map[string]string{
		"list fails": `case "$1 $2" in "s3api head-bucket") exit 0 ;; *) echo 'AccessDenied' >&2; exit 254 ;; esac`,
		"cp fails": `case "$1 $2" in "s3api head-bucket") exit 0 ;;
  "s3api list-objects-v2") echo 'development-platform/bedrock/terraform.tfstate' ;;
  *) echo 'AccessDenied' >&2; exit 254 ;; esac`,
		"not json": `case "$1 $2" in "s3api head-bucket") exit 0 ;;
  "s3api list-objects-v2") echo 'development-platform/bedrock/terraform.tfstate' ;;
  *) echo 'garbage' ;; esac`,
	} {
		t.Run(name, func(t *testing.T) {
			st, _ := apDestroyFakes(t, body)
			delete(st.Outputs, "kms_key_arn")
			err := DestroyAgentPlatform(context.Background(), st, AgentPlatformTeardown{})
			if err == nil || !strings.Contains(err.Error(), "could not be read") {
				t.Fatalf("unreadable state must not read as an empty tree: %v", err)
			}
		})
	}
}

// ─────────────────────────── outputs ───────────────────────────

// needOutput says which of three things happened, from the record captureOutputs keeps,
// instead of naming two causes it never checked.
func TestNeedOutput_CitesWhatHappenedToTheProducer(t *testing.T) {
	for _, tc := range []struct {
		name string
		errs map[string]error
		want string
	}{
		{"read failed", map[string]error{"secrets": errors.New("AccessDenied on the state bucket")}, "AccessDenied on the state bucket"},
		{"read, key absent", map[string]error{"secrets": nil}, "outputs were read and carry no"},
		{"never read", nil, "never read in this run"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, _ := apState(t)
			st.Runner.DryRun = false
			delete(st.Outputs, "kms_key_arn")
			st.CaptureErr = tc.errs
			_, err := needOutput(st, "kms_key_arn", "secrets")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error carrying %q, got %v", tc.want, err)
			}
		})
	}
}

// A failed capture is recorded and said, not swallowed.
func TestCaptureOutputs_RecordsAndSaysAFailedRead(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "terragrunt"), []byte(`#!/bin/sh
case "$2" in
  *secrets) echo 'Error: no state for this root' >&2; exit 1 ;;
  *network) exit 0 ;;
  *) echo '{"vpc_id":{"value":"vpc-1","type":"string"}}' ;;
esac
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var out strings.Builder
	st, _ := apState(t)
	st.Runner = exec.New(&out)
	st.Runner.ReadAttempts = 1
	st.Outputs = map[string]string{}

	captureOutputs(context.Background(), st, "secrets")
	captureOutputs(context.Background(), st, "network")
	captureOutputs(context.Background(), st, "cluster")

	if err := st.CaptureErr["secrets"]; err == nil || !strings.Contains(err.Error(), "no state for this root") {
		t.Errorf("the failed read must be recorded with its reason: %v", err)
	}
	if err := st.CaptureErr["network"]; err == nil || !strings.Contains(err.Error(), "printed nothing") {
		t.Errorf("an empty read is not a successful one: %v", err)
	}
	if err, read := st.CaptureErr["cluster"]; !read || err != nil {
		t.Errorf("a successful read is recorded as nil: read=%v err=%v", read, err)
	}
	if !strings.Contains(out.String(), "could not read secrets's terragrunt outputs") {
		t.Errorf("a failed capture must say so in one line:\n%s", out.String())
	}
}

// The apply path says the tree is untouched, which there is true, and no more than that: the
// failure is a plain one, so the rollback still unwinds what the earlier phases built, and a
// message saying nothing needs unwinding contradicted what happened next.
func TestApplyAgentPlatform_MissingOutputSaysNothingWasApplied(t *testing.T) {
	st, _ := apState(t)
	st.Runner.DryRun = false
	delete(st.Outputs, "vpc_id")
	err := applyAgentPlatform(context.Background(), st)
	if err == nil || !strings.Contains(err.Error(), "nothing of that tree to unwind") {
		t.Fatalf("the apply path must say nothing in the tree was applied: %v", err)
	}
	if strings.Contains(err.Error(), "nothing needs unwinding") {
		t.Errorf("a failure that rolls back must not say nothing needs unwinding: %v", err)
	}
	delete(st.Outputs, "kms_key_arn")
	st.Outputs["vpc_id"] = "vpc-0abc"
	if err := applyAgentPlatform(context.Background(), st); err == nil {
		t.Fatal("a missing account-chain output must fail the apply")
	}
}

// ─────────────────────────── destroy from state ───────────────────────────

// apStateAWS answers the state bucket as a tree whose listed keys hold the given number of
// resources, and the head-bucket as present.
func apStateAWS(held map[string]int) string {
	var keys, arms strings.Builder
	for key, n := range held {
		fmt.Fprintf(&keys, "%s\t", key)
		res := strings.TrimSuffix(strings.Repeat(`{"type":"aws_iam_role"},`, n), ",")
		fmt.Fprintf(&arms, "             */%s) echo '{\"resources\":[%s]}' ;;\n", key, res)
	}
	return fmt.Sprintf(`
case "$1 $2" in
  "s3api head-bucket")    exit 0 ;;
  "s3api list-objects-v2") printf '%s\n' ;;
  "s3 cp") case "$3" in
%s             *) echo '{"resources":[]}' ;;
           esac ;;
  *) exit 1 ;;
esac`, keys.String(), arms.String())
}

// destroyedRoots is the working directories terragrunt was asked to destroy.
func destroyedRoots(t *testing.T, logf string) []string {
	t.Helper()
	b, _ := os.ReadFile(logf)
	var roots []string
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) > 1 && slices.Contains(f, "destroy") && f[0] == "--working-dir" {
			roots = append(roots, f[1])
		}
	}
	return roots
}

// A config that turned the agent platform off after it was applied still has the tree
// standing. A destroy reads the tree's state and tears down what it holds; the config alone
// used to skip the tree, leaving its IAM, SSM and buckets and reporting a clean teardown.
func TestDestroyAgentPlatform_FromStateTearsDownATreeTheConfigTurnedOff(t *testing.T) {
	st, logf := apDestroyFakes(t, apStateAWS(map[string]int{
		"development-platform/bedrock/terraform.tfstate":     2,
		"development-platform/kill-switch/terraform.tfstate": 0,
		"org/bedrock-account/terraform.tfstate":              1,
	}))
	off := false
	st.Config.AgentPlatform.Enable = &off
	var out strings.Builder
	st.Runner.Out = &out

	if err := DestroyAgentPlatform(context.Background(), st, AgentPlatformTeardown{FromState: true}); err != nil {
		t.Fatalf("DestroyAgentPlatform: %v", err)
	}
	if got := destroyedRoots(t, logf); !slices.Equal(got, []string{"terraform/live/development-platform/bedrock"}) {
		t.Errorf("destroyed %v, want only bedrock — the root that holds state; an emptied root and an "+
			"account root this teardown does not reach are left alone", got)
	}
	// An account root this teardown does not reach is not announced as torn down.
	if strings.Contains(out.String(), "bedrock-account is not in this config") {
		t.Errorf("the teardown announced an account root it leaves standing:\n%s", out.String())
	}

	// Without FromState, a rollback's view: the config installs no tree, so nothing is touched.
	st2, logf2 := apDestroyFakes(t, apStateAWS(map[string]int{"development-platform/bedrock/terraform.tfstate": 2}))
	st2.Config.AgentPlatform.Enable = &off
	if err := DestroyAgentPlatform(context.Background(), st2, AgentPlatformTeardown{}); err != nil {
		t.Fatal(err)
	}
	if terragruntRan(logf2) {
		t.Error("a rollback with the platform off walked the tree")
	}
}

// With the cost tier turned off after it was applied, its per-cluster root is still standing
// and is torn down with the rest; the account root waits for --account-scoped.
func TestDestroyAgentPlatform_FromStateReachesACostTierTheConfigTurnedOff(t *testing.T) {
	st, logf := apDestroyFakes(t, apStateAWS(map[string]int{
		"development-platform/cost-access/terraform.tfstate": 3,
		"org/cost-pipeline/terraform.tfstate":                4,
	}))
	off := false
	st.Config.AgentPlatform.CostPipeline = &off

	if err := DestroyAgentPlatform(context.Background(), st, AgentPlatformTeardown{FromState: true}); err != nil {
		t.Fatalf("DestroyAgentPlatform: %v", err)
	}
	got := destroyedRoots(t, logf)
	if !slices.Contains(got, "terraform/live/development-platform/cost-access") || slices.Contains(got, "terraform/live/org/cost-pipeline") {
		t.Errorf("destroyed %v: cost-access holds state and must go; cost-pipeline is account-scoped", got)
	}
	if got[0] != "terraform/live/development-platform/cost-access" {
		t.Errorf("the walk is reverse apply order, so cost-access comes first: %v", got)
	}
}

// A disabled tree with nothing in it is nothing to destroy, and says so.
func TestDestroyAgentPlatform_FromStateWithNothingHeldTouchesNothing(t *testing.T) {
	st, logf := apDestroyFakes(t, apStateAWS(nil))
	var out strings.Builder
	st.Runner.Out = &out
	off := false
	st.Config.AgentPlatform.Enable = &off
	if err := DestroyAgentPlatform(context.Background(), st, AgentPlatformTeardown{FromState: true}); err != nil {
		t.Fatal(err)
	}
	if terragruntRan(logf) || !strings.Contains(out.String(), "nothing to destroy") {
		t.Errorf("an empty tree the config does not install is nothing to destroy:\n%s", out.String())
	}
}

// State that could not be listed is not a tree that holds nothing.
func TestDestroyAgentPlatform_FromStateUnlistableIsAnError(t *testing.T) {
	st, logf := apDestroyFakes(t, `case "$1 $2" in "s3api head-bucket") exit 0 ;; *) echo 'AccessDenied' >&2; exit 254 ;; esac`)
	off := false
	st.Config.AgentPlatform.Enable = &off
	err := DestroyAgentPlatform(context.Background(), st, AgentPlatformTeardown{FromState: true})
	if err == nil || !strings.Contains(err.Error(), "could not establish") {
		t.Fatalf("an unlistable state bucket must not read as an empty tree: %v", err)
	}
	if terragruntRan(logf) {
		t.Error("terragrunt ran over state nobody could read")
	}
}

// The account roots a teardown leaves without --account-scoped are named when they hold
// state, so a destroy that leaves them does not exit 0.
func TestAgentPlatformLeft_NamesTheAccountRootsLeftStanding(t *testing.T) {
	st, _ := apDestroyFakes(t, apStateAWS(map[string]int{
		"org/bedrock-account/terraform.tfstate":          1,
		"org/cost-pipeline/terraform.tfstate":            0,
		"development-platform/bedrock/terraform.tfstate": 5,
	}))
	left, err := AgentPlatformLeft(context.Background(), st, AgentPlatformTeardown{})
	if err != nil || !slices.Equal(left, []string{"bedrock-account (1 resource(s))"}) {
		t.Errorf("left = %v, %v; want the one account root that holds state", left, err)
	}
	if left, err := AgentPlatformLeft(context.Background(), st, AgentPlatformTeardown{AccountScoped: true}); left != nil || err != nil {
		t.Errorf("--account-scoped reaches the account roots, so none is left: %v, %v", left, err)
	}

	absent, _ := apDestroyFakes(t, `echo 'An error occurred (404) when calling the HeadBucket operation: Not Found' >&2; exit 254`)
	if left, err := AgentPlatformLeft(context.Background(), absent, AgentPlatformTeardown{}); left != nil || err != nil {
		t.Errorf("a tree never applied leaves nothing: %v, %v", left, err)
	}
	denied, _ := apDestroyFakes(t, `echo 'An error occurred (403) when calling the HeadBucket operation: Forbidden' >&2; exit 254`)
	if _, err := AgentPlatformLeft(context.Background(), denied, AgentPlatformTeardown{}); err == nil || !strings.Contains(err.Error(), "Forbidden") {
		t.Errorf("a bucket nobody could see is not a tree that left nothing: %v", err)
	}
}

// A config that installs every component of the tree has nothing outside it to read, so a
// destroy from state walks the config's own set without listing the bucket.
func TestDestroyAgentPlatform_FromStateWithEveryComponentConfiguredListsNothing(t *testing.T) {
	st, logf := apDestroyFakes(t, `case "$1 $2" in "s3api head-bucket") exit 0 ;; *) echo 'unexpected aws call' >&2; exit 254 ;; esac`)
	if err := DestroyAgentPlatform(context.Background(), st, AgentPlatformTeardown{FromState: true}); err != nil {
		t.Fatalf("DestroyAgentPlatform: %v", err)
	}
	var want int
	for _, c := range allAPComponents() {
		if !c.account {
			want++
		}
	}
	if got := destroyedRoots(t, logf); len(got) != want {
		t.Errorf("destroyed %v, want every cluster root of the config's own set", got)
	}
}
