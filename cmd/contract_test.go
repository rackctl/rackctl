package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/doctor"
	"github.com/rackctl/rackctl/internal/engine"
	"github.com/rackctl/rackctl/internal/tui"
)

// The exit codes are a contract that docs/exit-codes.md publishes and callers branch on.
// Renumbering one silently changes what a retry loop does, so the numbers are pinned here
// rather than left to whatever the const block happens to say.
func TestExitCodes_AreStableAndDistinct(t *testing.T) {
	want := map[string]int{
		"ExitOK": 0, "ExitFailure": 1, "ExitConfig": 2, "ExitPreflight": 3,
		"ExitDeclined": 4, "ExitStanding": 5, "ExitRolledBack": 6,
		"ExitStranded": 7, "ExitAborted": 8,
	}
	got := map[string]int{
		"ExitOK": ExitOK, "ExitFailure": ExitFailure, "ExitConfig": ExitConfig,
		"ExitPreflight": ExitPreflight, "ExitDeclined": ExitDeclined,
		"ExitStanding": ExitStanding, "ExitRolledBack": ExitRolledBack,
		"ExitStranded": ExitStranded, "ExitAborted": ExitAborted,
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s = %d, want %d — docs/exit-codes.md publishes these and callers branch on them", name, got[name], w)
		}
	}

	seen := map[int]string{}
	for name, code := range got {
		if other, dup := seen[code]; dup {
			t.Errorf("%s and %s both exit %d — two outcomes a caller must tell apart share a signal", name, other, code)
		}
		seen[code] = name
	}
}

// A nil error is success; an unclassified one is the generic failure. Anything else would
// make a passing run indistinguishable from a failed one.
func TestExitCode_NilIsSuccessAndUnclassifiedIsGeneric(t *testing.T) {
	if got := ExitCode(nil); got != ExitOK {
		t.Errorf("ExitCode(nil) = %d, want %d", got, ExitOK)
	}
	if got := ExitCode(errors.New("something")); got != ExitFailure {
		t.Errorf("an unclassified error must be the generic failure, got %d", got)
	}
}

// The classification survives wrapping. A command that adds context to an error must not
// lose the status the caller branches on.
func TestExitCode_SurvivesWrapping(t *testing.T) {
	err := fmt.Errorf("loading config: %w", withExit(ExitConfig, errors.New("bad yaml")))
	if got := ExitCode(err); got != ExitConfig {
		t.Errorf("a wrapped classification must survive, got %d want %d", got, ExitConfig)
	}
}

// The three engine outcomes a caller has to act on differently must not collapse. "The
// rollback ran and there is nothing to clean up", "the platform is up and usable" and
// "something may still be billing" call for opposite next moves.
func TestClassifyRun_SeparatesTheThreeCloudStates(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"clean", nil, ExitOK},
		{"rolled back", errors.New(`phase "cluster" failed: boom`), ExitRolledBack},
		{"left standing, no rollback", fmt.Errorf("phase %q failed: %w", "gitops",
			&engine.NoRollbackError{Err: errors.New("not converged")}), ExitStanding},
		{"left standing, optional", errors.New("optional phase(s) failed: portal"), ExitStanding},
		{"stranded", errors.New(`phase "substrate" failed: boom (and the rollback did not complete: x)`), ExitStranded},
		{"aborted", fmt.Errorf("run: %w", tui.ErrAborted), ExitAborted},
	} {
		if got := ExitCode(classifyRun(tc.err)); got != tc.want {
			t.Errorf("%s: exit %d, want %d", tc.name, got, tc.want)
		}
	}
}

// The failure name printed to stderr and the status an agent reads must describe the same
// outcome, or a terminal and a script disagree about what happened.
func TestClassify_NamesMatchTheirCodes(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{withExit(ExitStranded, errors.New("x")), "resources-may-remain (exit 7)"},
		{withExit(ExitPreflight, errors.New("x")), "preflight (exit 3)"},
		{errors.New("x"), "failure (exit 1)"},
	} {
		if got := Classify(tc.err); got != tc.want {
			t.Errorf("Classify = %q, want %q", got, tc.want)
		}
	}
}

// The JSON report is a published shape. Fields may be added; renaming or removing one
// breaks every caller that reads it.
func TestCheckJSON_ShapeIsStable(t *testing.T) {
	cfg := config.Default()
	cfg.Cluster.Name = "platform"
	cfg.Cloud.AccountID = "111111111111"
	cfg.Cloud.Region = "us-east-1"

	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	emitErr := emitCheckJSON(cfg, "asserted",
		[]doctor.Result{{Name: "vcpu quota", Status: doctor.Fail, Detail: "32 vCPU"}},
		[]doctor.Result{{Name: "applications", Status: doctor.OK, Detail: "44 Healthy"}},
		true)
	_ = w.Close()
	os.Stdout = old
	if emitErr != nil {
		t.Fatalf("emit: %v", emitErr)
	}

	var buf strings.Builder
	b := make([]byte, 8192)
	for {
		n, err := r.Read(b)
		buf.Write(b[:n])
		if err != nil {
			break
		}
	}

	var doc map[string]any
	if err := json.Unmarshal([]byte(buf.String()), &doc); err != nil {
		t.Fatalf("the report must be valid JSON: %v\n%s", err, buf.String())
	}
	for _, key := range []string{"cluster", "environment", "account", "region",
		"clusterHealth", "passed", "preflight", "platform"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("the report lost the %q field — docs/exit-codes.md publishes this shape", key)
		}
	}
	if doc["passed"] != false {
		t.Errorf("passed must reflect the failure, got %v", doc["passed"])
	}
	// clusterHealth is what tells a caller whether an empty platform list means "no
	// findings" or "nobody looked". Without it the two are the same document.
	if doc["clusterHealth"] != "asserted" {
		t.Errorf("clusterHealth = %v, want asserted", doc["clusterHealth"])
	}
	pre, _ := doc["preflight"].([]any)
	if len(pre) != 1 {
		t.Fatalf("preflight results did not survive: %v", doc["preflight"])
	}
	first, _ := pre[0].(map[string]any)
	if first["status"] != "fail" {
		t.Errorf("status must be the lowercase machine token, got %v", first["status"])
	}
}
