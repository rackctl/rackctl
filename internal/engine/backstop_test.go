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
	var out bytes.Buffer
	run := exec.New(&out)
	run.DryRun = true
	t.Setenv("PATH", t.TempDir()) // no aws: the IAM and Karpenter enumerations cannot run

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
