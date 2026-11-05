package phases

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/engine"
	"github.com/rackctl/rackctl/internal/exec"
)

// substrateTeardownLog runs substrate.Teardown against a fake terragrunt that records
// "<verb> <component> force=<TF_VAR_force_destroy_buckets>" per invocation, failing every
// destroy of failDestroy. Every component in applied is recorded as created by this run.
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
		Outputs: map[string]string{}, Applied: applied, Created: applied}
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

// fakeStateList puts a fake terragrunt on PATH whose `state list` prints listing for every
// component, logging "<verb> <component>" per invocation, and returns the log path.
func fakeStateList(t *testing.T, listing string) string {
	t.Helper()
	dir := t.TempDir()
	logf := filepath.Join(dir, "invocations")
	script := fmt.Sprintf(`#!/bin/sh
for a in "$@"; do
  case "$a" in apply|destroy|init|list) verb="$a";; esac
  case "$a" in live/*) comp="${a##*/}";; esac
done
echo "$verb $comp" >> %q
[ "$verb" = list ] && printf '%%s' %q
exit 0
`, logf, listing)
	if err := os.WriteFile(filepath.Join(dir, "terragrunt"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logf
}

// stagingApplyState is a State that applies for real outside development.
func stagingApplyState(t *testing.T) (*engine.State, *strings.Builder) {
	t.Helper()
	st, out := apState(t)
	st.Runner.DryRun = false
	st.Config.Environment = config.EnvStaging
	return st, out
}

// A bucket-owning component counts as created by this run only when its state was empty
// before the run first applied it. An apply over state an earlier platform left behind
// adopts its buckets, and a later re-apply in the same run does not overwrite the record.
func TestApply_RecordsCreatedOnlyOverAnEmptyState(t *testing.T) {
	for _, c := range []struct {
		name, listing string
		want          bool
	}{
		{"empty state", "", true},
		{"state already held", "aws_s3_bucket.access_logs\naws_s3_bucket.model_artifacts\n", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			logf := fakeStateList(t, c.listing)
			st, out := stagingApplyState(t)
			if err := apply(context.Background(), st, "agent-iam"); err != nil {
				t.Fatal(err)
			}
			if !st.Applied["agent-iam"] || st.Created["agent-iam"] != c.want {
				t.Fatalf("applied=%v created=%v, want created=%v", st.Applied, st.Created, c.want)
			}
			if !c.want && !strings.Contains(out.String(), "agent-iam already holds state") {
				t.Errorf("an adopted state must be said:\n%s", out.String())
			}
			b, _ := os.ReadFile(logf)
			log := strings.Fields(string(b))
			if !slices.Contains(log, "list") || slices.Index(log, "list") > slices.Index(log, "apply") {
				t.Errorf("state must be listed before the apply: %v", log)
			}
			// A second apply in the same run, now over state it wrote, keeps the record.
			if err := apply(context.Background(), st, "agent-iam"); err != nil {
				t.Fatal(err)
			}
			if st.Created["agent-iam"] != c.want {
				t.Errorf("a re-apply changed the record to %v", st.Created["agent-iam"])
			}
		})
	}
}

// Nothing is read where a rollback could not act on it: in development, for a component
// that owns no buckets, or in a dry-run.
func TestApply_ReadsStateOnlyWhereARollbackCouldPermit(t *testing.T) {
	for name, mut := range map[string]func(*engine.State) (component string){
		"development": func(st *engine.State) string { st.Config.Environment = config.EnvDev; return "agent-iam" },
		"no buckets":  func(*engine.State) string { return "secrets" },
		"dry-run":     func(st *engine.State) string { st.Runner.DryRun = true; return "agent-iam" },
	} {
		t.Run(name, func(t *testing.T) {
			logf := fakeStateList(t, "")
			st, _ := stagingApplyState(t)
			comp := mut(st)
			if err := apply(context.Background(), st, comp); err != nil {
				t.Fatal(err)
			}
			b, _ := os.ReadFile(logf)
			if strings.Contains(string(b), "list") {
				t.Errorf("state was read: %s", b)
			}
			if st.Created[comp] {
				t.Errorf("%s was recorded as created without proof", comp)
			}
		})
	}
}

// A state that cannot be read is treated as holding something.
func TestApply_AnUnreadableStateIsNotProofOfCreation(t *testing.T) {
	for name, fail := range map[string]string{"init": "init", "state list": "list"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			// The first invocation of the failing verb fails; tg's own init, which follows, does not.
			script := fmt.Sprintf(`#!/bin/sh
for a in "$@"; do case "$a" in apply|init|list) verb="$a";; esac; done
if [ "$verb" = %q ] && [ ! -e %q ]; then : > %q; echo 'AccessDenied' >&2; exit 1; fi
exit 0
`, fail, filepath.Join(dir, "failed"), filepath.Join(dir, "failed"))
			if err := os.WriteFile(filepath.Join(dir, "terragrunt"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			st, out := stagingApplyState(t)
			st.Runner.ReadAttempts = 1
			if err := apply(context.Background(), st, "agent-iam"); err != nil {
				t.Fatal(err)
			}
			if st.Created["agent-iam"] {
				t.Error("an unreadable state was taken as proof of creation")
			}
			if !strings.Contains(out.String(), "could not read agent-iam's state") {
				t.Errorf("the unread state must be said:\n%s", out.String())
			}
		})
	}
}

// A rollback never permits force_destroy on a component this run applied over state that
// was already there: its buckets and Aurora may hold an earlier platform's data. It names
// the component and the deliberate remedy instead.
func TestSubstrateTeardown_DoesNotPermitWhatThisRunAdopted(t *testing.T) {
	cfg := baseCfg()
	cfg.Environment = config.EnvStaging
	adopted := ForceDestroyBucketComponents(cfg)[0]
	dir := t.TempDir()
	logf := filepath.Join(dir, "invocations")
	script := fmt.Sprintf(`#!/bin/sh
for a in "$@"; do case "$a" in apply|destroy) verb="$a";; esac; case "$a" in live/*) comp="${a##*/}";; esac; done
[ -n "$verb" ] && echo "$verb $comp" >> %q
exit 0
`, logf)
	if err := os.WriteFile(filepath.Join(dir, "terragrunt"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var out strings.Builder
	cfg.Cluster.Name = "platform"
	cfg.Cloud.AccountID = "111111111111"
	cfg.Cloud.Region = "us-west-2"
	st := &engine.State{Config: cfg, Runner: exec.New(&out), Repos: engine.Repos{LandingZone: dir},
		Outputs: map[string]string{}, Applied: map[string]bool{adopted: true},
		Created: map[string]bool{adopted: false}}
	_ = (substrate{}).Teardown(context.Background(), st)
	b, _ := os.ReadFile(logf)
	if strings.Contains(string(b), "apply ") {
		t.Fatalf("the rollback applied force_destroy on adopted state:\n%s", b)
	}
	if strings.Contains(out.String(), "this DELETES the data in them") {
		t.Errorf("a permit was announced for adopted state:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "["+adopted+"] own buckets") ||
		!strings.Contains(out.String(), "may predate it") ||
		!strings.Contains(out.String(), "rackctl destroy --force-buckets") {
		t.Errorf("the adopted component must be named with the remedy:\n%s", out.String())
	}
}
