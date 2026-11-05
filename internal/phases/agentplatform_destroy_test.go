package phases

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

// The apply path still says the tree is untouched, which there is true.
func TestApplyAgentPlatform_MissingOutputSaysNothingWasApplied(t *testing.T) {
	st, _ := apState(t)
	st.Runner.DryRun = false
	delete(st.Outputs, "vpc_id")
	err := applyAgentPlatform(context.Background(), st)
	if err == nil || !strings.Contains(err.Error(), "nothing needs unwinding") {
		t.Fatalf("the apply path must say nothing in the tree was applied: %v", err)
	}
	delete(st.Outputs, "kms_key_arn")
	st.Outputs["vpc_id"] = "vpc-0abc"
	if err := applyAgentPlatform(context.Background(), st); err == nil {
		t.Fatal("a missing account-chain output must fail the apply")
	}
}
