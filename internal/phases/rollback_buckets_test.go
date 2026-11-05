package phases

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/engine"
	"github.com/rackctl/rackctl/internal/exec"
)

// substrateTeardownLog runs substrate.Teardown against a fake terragrunt that records
// "<verb> <component> force=<TF_VAR_force_destroy_buckets>" per invocation, failing every
// destroy of failDestroy.
func substrateTeardownLog(t *testing.T, env config.Environment, applied map[string]bool, failDestroy string, failApply ...string) ([]string, string, error) {
	t.Helper()
	dir := t.TempDir()
	logf := filepath.Join(dir, "invocations")
	script := fmt.Sprintf(`#!/bin/sh
for a in "$@"; do
  case "$a" in apply|destroy) verb="$a";; esac
  case "$a" in live/*) comp="${a##*/}";; esac
done
[ -n "$verb" ] || exit 0
echo "$verb $comp force=${TF_VAR_force_destroy_buckets:-UNSET}" >> %q
[ "$verb $comp" = "destroy %s" ] && { echo 'BucketNotEmpty' >&2; exit 1; }
[ "$verb $comp" = "apply %s" ] && { echo 'AccessDenied' >&2; exit 1; }
exit 0
`, logf, failDestroy, strings.Join(failApply, ""))
	if err := os.WriteFile(filepath.Join(dir, "terragrunt"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var out strings.Builder
	cfg := baseCfg()
	cfg.Environment = env
	cfg.Cluster.Name = "platform"
	cfg.Cloud.AccountID = "111111111111"
	cfg.Cloud.Region = "us-west-2"
	st := &engine.State{Config: cfg, Runner: exec.New(&out), Repos: engine.Repos{LandingZone: dir},
		Outputs: map[string]string{}, Applied: applied}
	err := (substrate{}).Teardown(context.Background(), st)
	b, _ := os.ReadFile(logf)
	return strings.Split(strings.TrimSpace(string(b)), "\n"), out.String(), err
}

// Outside development a rollback permits force_destroy before it destroys, but only on the
// bucket-owning components this run applied: permitting is an apply, and on a component the
// run did not build it would create one in the middle of the rollback. What it does not
// permit it names, with the remedy.
func TestSubstrateTeardown_PermitsBucketTeardownOnlyForWhatThisRunApplied(t *testing.T) {
	cfg := baseCfg()
	cfg.Environment = config.EnvStaging
	buckets := ForceDestroyBucketComponents(cfg)
	if len(buckets) < 2 {
		t.Fatalf("the fixture needs two bucket-owning components, got %v", buckets)
	}
	ours, notOurs := buckets[0], buckets[1]
	log, out, err := substrateTeardownLog(t, config.EnvStaging, map[string]bool{ours: true}, notOurs)

	permitAt, destroyAt := -1, -1
	for i, l := range log {
		switch {
		case l == "apply "+ours+" force=true":
			permitAt = i
		case strings.HasPrefix(l, "apply "+notOurs):
			t.Errorf("the rollback applied %s, which this run did not apply: %v", notOurs, log)
		case destroyAt < 0 && strings.HasPrefix(l, "destroy "):
			destroyAt = i
		}
	}
	if permitAt < 0 || permitAt > destroyAt {
		t.Fatalf("force_destroy must be applied on %s before the first destroy: %v", ours, log)
	}
	if !strings.Contains(out, "this DELETES the data in them") {
		t.Errorf("the permit must be said before it acts:\n%s", out)
	}
	if !strings.Contains(out, "["+notOurs+"] own buckets") {
		t.Errorf("the component left unpermitted must be named:\n%s", out)
	}
	if err == nil || !strings.Contains(err.Error(), "rackctl destroy --force-buckets") {
		t.Errorf("an incomplete rollback must name the remedy: %v", err)
	}
}

// Development always allows teardown, so a rollback there applies nothing.
func TestSubstrateTeardown_DevelopmentPermitsNothing(t *testing.T) {
	cfg := baseCfg()
	cfg.Environment = config.EnvDev
	applied := map[string]bool{}
	for _, c := range ForceDestroyBucketComponents(cfg) {
		applied[c] = true
	}
	log, _, err := substrateTeardownLog(t, config.EnvDev, applied, "")
	if err != nil {
		t.Fatalf("teardown: %v", err)
	}
	for _, l := range log {
		if strings.HasPrefix(l, "apply ") {
			t.Fatalf("a development rollback applied something: %v", log)
		}
	}
}

// What a successful apply records is what the rollback may permit.
func TestApply_RecordsWhatThisRunApplied(t *testing.T) {
	st, _ := apState(t)
	st.Runner.DryRun = true
	if err := apply(context.Background(), st, "agent-iam"); err != nil {
		t.Fatal(err)
	}
	if !st.Applied["agent-iam"] {
		t.Fatal("a successful apply must be recorded")
	}
}

// With nothing this run applied there is nothing to permit; a permit that fails is said and
// the destroy still runs, since it is where a bucket that really refuses has to say so.
func TestSubstrateTeardown_APermitThatCannotRunDoesNotStopTheRollback(t *testing.T) {
	cfg := baseCfg()
	cfg.Environment = config.EnvStaging
	ours := ForceDestroyBucketComponents(cfg)[0]

	log, out, _ := substrateTeardownLog(t, config.EnvStaging, nil, "")
	for _, l := range log {
		if strings.HasPrefix(l, "apply ") {
			t.Fatalf("nothing was applied by this run, and the rollback applied: %v", log)
		}
	}
	if strings.Contains(out, "this DELETES the data in them") {
		t.Errorf("a permit was announced with nothing to permit:\n%s", out)
	}

	log, out, _ = substrateTeardownLog(t, config.EnvStaging, map[string]bool{ours: true}, "", ours)
	if !strings.Contains(out, "rollback: could not permit "+ours) {
		t.Errorf("a permit that failed must be said:\n%s", out)
	}
	destroyed := false
	for _, l := range log {
		destroyed = destroyed || strings.HasPrefix(l, "destroy "+ours)
	}
	if !destroyed {
		t.Errorf("the destroy must still run after a failed permit: %v", log)
	}
}
