package cmd

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// End-to-end over the built binary: config on disk, stubbed tools on PATH, real argv.
//
// TestCheckJSON_ShapeIsStable calls emitCheckJSON directly, which proves the encoder and
// nothing else — it never runs the switch that decides clusterHealth, never routes prose to
// stderr, and never exercises an exit status. Running the binary is what pins the contract
// an agent actually consumes: stdout is the document alone, stderr carries the humans, and
// $? classifies.
func buildRackctl(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "rackctl")
	build := exec.Command("go", "build", "-o", bin, "..")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

// stubTools puts every tool `check` requires on PATH, each failing. The point is the
// REPORT's shape under a cluster nobody can reach, which is the case an agent meets most.
func stubTools(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, tool := range []string{"tofu", "terragrunt", "kubectl", "helm", "aws", "git", "gh"} {
		script := "#!/bin/sh\necho 'An error occurred (AccessDeniedException)' >&2\nexit 254\n"
		if err := os.WriteFile(filepath.Join(dir, tool), []byte(script), 0o755); err != nil {
			t.Fatalf("stub %s: %v", tool, err)
		}
	}
	return dir
}

func TestCheckJSON_EndToEnd(t *testing.T) {
	bin := buildRackctl(t)
	stubs := stubTools(t)

	cfg := filepath.Join(t.TempDir(), "rackctl.yaml")
	src, err := os.ReadFile(filepath.Join("..", "examples", "rackctl.yaml"))
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	if err := os.WriteFile(cfg, src, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cmd := exec.Command(bin, "check", "-c", cfg, "--output", "json")
	cmd.Env = append(os.Environ(),
		"PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOME="+t.TempDir())
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()

	// stdout is the document ALONE. A banner or a warning here means the caller has to
	// filter its own input, which is the thing --output json exists to remove.
	var doc map[string]any
	if err := json.Unmarshal([]byte(stdout.String()), &doc); err != nil {
		t.Fatalf("stdout is not a bare JSON document (%v):\n%s", err, stdout.String())
	}

	if doc["clusterHealth"] != "unreachable" {
		t.Errorf("clusterHealth = %v, want \"unreachable\" — every tool fails here, so nothing "+
			"could have been asserted about the platform", doc["clusterHealth"])
	}
	if doc["passed"] != false {
		t.Errorf("passed = %v, want false", doc["passed"])
	}
	pre, _ := doc["preflight"].([]any)
	if len(pre) == 0 {
		t.Error("no preflight results reached the document — a report with an empty list and " +
			"clusterHealth set is indistinguishable from a healthy one")
	}
	if plat, _ := doc["platform"].([]any); len(plat) != 0 {
		t.Errorf("platform carries %d results against an unreachable cluster", len(plat))
	}

	// The tool's own reason must survive into a verdict. Every stub answers AccessDenied,
	// so at least one detail has to name it — that is the discarded-diagnostic invariant,
	// asserted through the binary rather than through a unit.
	if !strings.Contains(stdout.String(), "AccessDenied") {
		t.Errorf("no verdict carries the reason the tools gave, so the operator is sent to "+
			"find out what rackctl already knew:\n%s", stdout.String())
	}

	// Human prose goes to stderr, and the failed run classifies rather than exiting 1.
	if !strings.Contains(stderr.String(), "rackctl check") {
		t.Errorf("the banner did not reach stderr:\n%s", stderr.String())
	}
	if runErr == nil {
		t.Fatal("a check with every tool failing must exit non-zero")
	}
	ee, ok := runErr.(*exec.ExitError)
	if !ok {
		t.Fatalf("unexpected error type: %v", runErr)
	}
	if got := ee.ExitCode(); got != ExitFailure {
		t.Errorf("exit %d, want %d — docs/exit-codes.md publishes these", got, ExitFailure)
	}
}

// An invalid --output value is a config error, not a generic failure. An agent branching
// on the status must be able to tell "you asked for something I do not emit" from "the
// platform is unhealthy".
func TestCheckJSON_BadOutputValueIsAConfigError(t *testing.T) {
	bin := buildRackctl(t)

	cmd := exec.Command(bin, "check", "-c", "nonexistent.yaml", "--output", "yaml")
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
	err := cmd.Run()

	ee, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("want a non-zero exit, got %v", err)
	}
	if got := ee.ExitCode(); got != ExitConfig {
		t.Errorf("exit %d, want %d (config)", got, ExitConfig)
	}
}
