package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/exec"
	"github.com/rackctl/rackctl/internal/reap"
)

// sweepPhase is a phase whose teardown runs a backstop of its own, the way cluster.Teardown
// runs the volume sweep, and records it on State.
type sweepPhase struct {
	recPhase
	b reap.Backstop
}

func (p sweepPhase) Teardown(_ context.Context, st *State) error {
	st.Backstops = append(st.Backstops, p.b)
	return nil
}

// A rollback ends by saying which backstops ran, found nothing, failed or did not run —
// its own sweeps and the ones a phase teardown ran — instead of leaving that in the scroll.
func TestRollbackEndsWithTheBackstopSummary(t *testing.T) {
	assumePlatform(t, PlatformAbsent)
	var out bytes.Buffer
	run := exec.New(&out)
	run.DryRun = true
	reachableCluster(t) // aws refuses the IAM and Karpenter enumerations

	eng := &Engine{
		Phases: []Phase{
			sweepPhase{
				recPhase: recPhase{id: "cluster", enabled: true, log: new([]string)},
				b:        reap.Backstop{Name: "orphaned EBS volumes", Outcome: reap.Failed, Detail: "could not delete vol-1"},
			},
			recPhase{id: "gitops", enabled: true, fail: true, log: new([]string)},
		},
		Out:         &out,
		CleanOnFail: true,
	}
	st := &State{Runner: run, Config: &config.Config{}, KubeconfigCluster: "development-platform"}
	_ = eng.Run(context.Background(), st)

	got := out.String()
	summary := strings.Index(got, "backstop summary")
	if summary < 0 {
		t.Fatalf("the rollback printed no backstop summary:\n%s", got)
	}
	for _, want := range []string{
		"in-cluster reap: ran",
		"operator IAM roles: did NOT run",
		"Karpenter nodes: did NOT run",
		"orphaned EBS volumes: FAILED — could not delete vol-1",
		"not every backstop ran clean",
	} {
		if i := strings.Index(got, want); i < summary {
			t.Errorf("the summary is missing %q:\n%s", want, got)
		}
	}
	if len(st.Backstops) != 5 {
		t.Errorf("want the four rollback sweeps and the phase's own, got %d: %+v", len(st.Backstops), st.Backstops)
	}
}

// rollbackWith runs a two-phase rollback whose cluster teardown records b, with no cluster
// this run pointed kubectl at, so b is the only backstop that can decide the outcome.
func rollbackWith(t *testing.T, b reap.Backstop) (string, error) {
	t.Helper()
	assumePlatform(t, PlatformAbsent)
	var out bytes.Buffer
	run := exec.New(&out)
	run.DryRun = true
	t.Setenv("PATH", t.TempDir())
	eng := &Engine{
		Phases: []Phase{
			sweepPhase{recPhase: recPhase{id: "cluster", enabled: true, log: new([]string)}, b: b},
			recPhase{id: "gitops", enabled: true, fail: true, log: new([]string)},
		},
		Out:         &out,
		CleanOnFail: true,
	}
	err := eng.Run(context.Background(), &State{Runner: run, Config: &config.Config{}})
	if err == nil {
		t.Fatal("a failed phase must fail the run")
	}
	return out.String(), err
}

// A rollback whose guard failed did not complete: the Karpenter node or volume it missed is
// still billing, so the run must not read as rolled back with nothing remaining.
func TestRollback_AGuardThatDidNotHoldIsAnIncompleteRollback(t *testing.T) {
	for _, o := range []reap.Outcome{reap.Failed, reap.NotRun} {
		out, err := rollbackWith(t, reap.Backstop{Name: "orphaned EBS volumes", Outcome: o})
		if !strings.Contains(err.Error(), "the rollback did not complete") ||
			!strings.Contains(err.Error(), "these backstops did not hold: orphaned EBS volumes") {
			t.Errorf("%s: the rollback must report itself incomplete, naming the guard: %v\n%s", o, err, out)
		}
	}
}

// Informational reports and guards that held leave a rollback complete.
func TestRollback_GuardsThatHeldLeaveTheRollbackComplete(t *testing.T) {
	for _, b := range []reap.Backstop{
		{Name: "orphaned EBS volumes", Outcome: reap.FoundNothing},
		{Name: "unprovable EBS volumes", Outcome: reap.NotRun, Informational: true},
	} {
		out, err := rollbackWith(t, b)
		if strings.Contains(err.Error(), "did not complete") {
			t.Errorf("%s %s: a rollback whose guards held is complete: %v\n%s", b.Name, b.Outcome, err, out)
		}
	}
}
