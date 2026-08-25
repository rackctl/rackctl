package phases

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A contract test against the REAL terragrunt, for the argv rackctl builds.
//
// Every other test in this package asserts the argv rackctl PRODUCES, against a fake
// terragrunt on PATH. That proves rackctl is internally consistent and nothing else: if
// terragrunt renamed --working-dir, every one of those tests would still pass and every
// apply would fail. The tool is the other half of the contract, and only the tool can
// answer for it.
//
// The consequence of being wrong is not cosmetic. tg() runs `init` before every verb, so a
// rejected global flag fails the first component of every phase; and the specific claim
// this pins — that the OLD post-command --terragrunt-working-dir is silently ignored — is
// worse than a rejection, because terragrunt then runs in the CURRENT directory against
// whatever tree happens to be there.
//
// Skipping is allowed on a developer machine and FORBIDDEN where this gates. CI installs
// terragrunt and sets RACKCTL_REQUIRE_TOOL_CONTRACTS, which turns the skip into a failure —
// otherwise the one test that reaches the real tool would print a skip into a green job, and
// nobody reads a green job. A contract test that never runs where it gates asserts nothing.
func terragruntOrSkip(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("terragrunt")
	if err != nil {
		if os.Getenv("RACKCTL_REQUIRE_TOOL_CONTRACTS") != "" {
			t.Fatal("terragrunt is not installed, and RACKCTL_REQUIRE_TOOL_CONTRACTS is set. " +
				"This test asserts the real tool's flag contract; skipping it here would " +
				"leave every terragrunt assertion in this repo checking only that rackctl " +
				"agrees with itself.")
		}
		t.Skip("terragrunt is not installed; this test asserts the real tool's flag contract")
	}
	return bin
}

// fixture writes the smallest tree terragrunt will act on.
func fixture(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "tree")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("terragrunt.hcl", "terraform {\n  source = \".\"\n}\n")
	write("main.tf", "output \"x\" { value = \"y\" }\n")
	return dir
}

// The global flags must be accepted BEFORE the command, which is the order tg() builds.
func TestTerragruntContract_GlobalFlagsPrecedeTheCommand(t *testing.T) {
	bin := terragruntOrSkip(t)
	dir := fixture(t)

	cmd := exec.Command(bin, "--working-dir", dir, "--non-interactive", "init")
	cmd.Dir = t.TempDir() // deliberately NOT the tree: --working-dir must be what decides
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the argv rackctl builds was rejected by terragrunt — every phase's init "+
			"fails on this: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "initialized") && !strings.Contains(string(out), "Initializing") {
		t.Errorf("init did not report initializing; the flags may be parsed but ignored:\n%s", out)
	}
}

// -auto-approve is a tofu flag and must be accepted AFTER the command.
func TestTerragruntContract_AutoApproveFollowsTheCommand(t *testing.T) {
	bin := terragruntOrSkip(t)
	dir := fixture(t)

	if out, err := exec.Command(bin, "--working-dir", dir, "--non-interactive", "init").CombinedOutput(); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	cmd := exec.Command(bin, "--working-dir", dir, "--non-interactive", "apply", "-auto-approve")
	cmd.Dir = t.TempDir()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("apply -auto-approve was rejected — every provisioning verb fails on this: "+
			"%v\n%s", err, out)
	}
}

// The claim that earns this file: the OLD post-command --terragrunt-working-dir does NOT
// select the directory. Being wrong here is worse than a rejected flag — terragrunt runs in
// the current directory against whatever tree is there.
func TestTerragruntContract_TheOldWorkingDirFlagDoesNotSelectTheDirectory(t *testing.T) {
	bin := terragruntOrSkip(t)
	dir := fixture(t)
	elsewhere := t.TempDir()

	cmd := exec.Command(bin, "init", "--terragrunt-working-dir", dir)
	cmd.Dir = elsewhere
	out, _ := cmd.CombinedOutput()

	// It looked for a terragrunt.hcl in the CWD rather than in dir, which is the whole
	// point: the old flag is not honoured, so the command silently acts on the wrong tree.
	if !strings.Contains(string(out), elsewhere) {
		// Not a skip where this gates: a version that stopped reporting the path is a
		// version whose behaviour is unknown, and unknown must not read as green.
		if os.Getenv("RACKCTL_REQUIRE_TOOL_CONTRACTS") != "" {
			t.Fatalf("terragrunt did not report a path under the working directory, so the "+
				"claim in tg() cannot be checked against this version:\n%s", out)
		}
		t.Skipf("terragrunt did not report a path under the working directory, so this "+
			"version may honour the old flag — rackctl's ordering comment needs re-checking "+
			"against it:\n%s", out)
	}
	if strings.Contains(string(out), dir) && !strings.Contains(string(out), elsewhere) {
		t.Errorf("the old flag selected the directory after all, so the comment in tg() is "+
			"wrong for this version:\n%s", out)
	}
}
