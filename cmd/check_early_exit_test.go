package cmd

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// runCheckJSON runs `check --output json` with the given PATH and config, and returns the
// parsed stdout document, stderr and the exit status.
func runCheckJSON(t *testing.T, path, cfg string) (map[string]any, string, int) {
	t.Helper()
	bin := buildRackctl(t)
	cmd := exec.Command(bin, "check", "-c", cfg, "--output", "json")
	cmd.Env = append(os.Environ(), "PATH="+path, "HOME="+t.TempDir())
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(stdout.String()), &doc); err != nil {
		t.Fatalf("a check that stopped early must still write a bare JSON document to stdout (%v):\n%s\nstderr:\n%s",
			err, stdout.String(), stderr.String())
	}
	return doc, stderr.String(), code
}

// exampleConfig writes the example config, with each old→new replacement applied. A
// replacement that matches nothing fails the test: a fixture that did not land proves nothing.
func exampleConfig(t *testing.T, replace ...string) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "examples", "rackctl.yaml"))
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	s := string(src)
	for i := 0; i+1 < len(replace); i += 2 {
		if !strings.Contains(s, replace[i]) {
			t.Fatalf("fixture did not land: the example carries no %q", replace[i])
		}
		s = strings.Replace(s, replace[i], replace[i+1], 1)
	}
	cfg := filepath.Join(t.TempDir(), "rackctl.yaml")
	if err := os.WriteFile(cfg, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func assertNotAsserted(t *testing.T, doc map[string]any, errWant string) {
	t.Helper()
	if doc["clusterHealth"] != "not-asserted" {
		t.Errorf("clusterHealth = %v, want not-asserted — nothing about the platform was looked at", doc["clusterHealth"])
	}
	if doc["passed"] != false {
		t.Errorf("passed = %v, want false for a check that did not run", doc["passed"])
	}
	if e, _ := doc["error"].(string); !strings.Contains(e, errWant) {
		t.Errorf("error = %q, want it to carry %q", e, errWant)
	}
	for _, key := range []string{"preflight", "platform"} {
		if _, ok := doc[key].([]any); !ok {
			t.Errorf("%s must be a list, even an empty one, got %#v", key, doc[key])
		}
	}
}

// A tool missing from PATH stopped the check before it wrote anything, so a caller piping
// stdout into a parser got an empty stream.
func TestCheckJSON_AMissingToolStillWritesTheDocument(t *testing.T) {
	stubs := stubTools(t)
	if err := os.Remove(filepath.Join(stubs, "helm")); err != nil {
		t.Fatal(err)
	}
	doc, stderr, code := runCheckJSON(t, stubs, exampleConfig(t))
	assertNotAsserted(t, doc, "helm")
	if pre, _ := doc["preflight"].([]any); len(pre) != 0 {
		t.Errorf("no preflight check ran, got %d results", len(pre))
	}
	if !strings.Contains(stderr, "missing required tools") {
		t.Errorf("the reason must reach stderr too:\n%s", stderr)
	}
	if code != ExitFailure {
		t.Errorf("exit %d, want %d — the status is unchanged", code, ExitFailure)
	}
}

// An identity that does not resolve stops the check the same way. With cloud.assumeRole
// set the check first assumes the role, and every stub answers AccessDenied.
func TestCheckJSON_AnUnresolvableIdentityStillWritesTheDocument(t *testing.T) {
	cfg := exampleConfig(t, "  profile: workload-development  # AWS SSO profile\n",
		"  profile: workload-development\n  assumeRole:\n    roleArn: arn:aws:iam::111111111111:role/rackctl-deploy\n")
	doc, stderr, code := runCheckJSON(t, stubTools(t), cfg)
	assertNotAsserted(t, doc, "AccessDenied")
	if !strings.Contains(stderr, "could not resolve the identity") {
		t.Errorf("the reason must reach stderr:\n%s", stderr)
	}
	if code != ExitFailure {
		t.Errorf("exit %d, want %d — the status is unchanged", code, ExitFailure)
	}
}

// A config that does not load still answers with a document, and keeps its config status.
func TestCheckJSON_AConfigThatDoesNotLoadStillWritesTheDocument(t *testing.T) {
	doc, _, code := runCheckJSON(t, stubTools(t), filepath.Join(t.TempDir(), "absent.yaml"))
	assertNotAsserted(t, doc, "absent.yaml")
	if code != ExitConfig {
		t.Errorf("exit %d, want %d (config)", code, ExitConfig)
	}
}

// Every jq path the docs tell an operator to run must name a field the report carries. The
// runbook's `.results[]` named one that never existed, so the documented command printed
// nothing and read as "no failures".
func TestDocumentedJQPaths_NameFieldsTheReportCarries(t *testing.T) {
	fields := map[string]bool{}
	for _, typ := range []reflect.Type{reflect.TypeOf(checkReport{}), reflect.TypeOf(checkResult{})} {
		for i := 0; i < typ.NumField(); i++ {
			name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
			fields[name] = true
		}
	}

	jq := regexp.MustCompile(`\| jq '([^']*)'`)
	field := regexp.MustCompile(`\.([A-Za-z_][A-Za-z0-9_]*)`)
	var checked int
	for _, doc := range []string{"../README.md", "../AGENTS.md", "../docs/runbook.md", "../docs/exit-codes.md"} {
		b, err := os.ReadFile(doc)
		if err != nil {
			t.Fatalf("read %s: %v", doc, err)
		}
		for _, m := range jq.FindAllStringSubmatch(string(b), -1) {
			for _, f := range field.FindAllStringSubmatch(m[1], -1) {
				checked++
				if !fields[f[1]] {
					t.Errorf("%s runs jq %q, and .%s is not a field of the check report", doc, m[1], f[1])
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no documented jq path was found — a test that examines nothing passes over nothing")
	}
}
