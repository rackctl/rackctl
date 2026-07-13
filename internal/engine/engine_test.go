package engine

import (
	"context"
	"errors"
	"io"
	"testing"
)

// recPhase records run/teardown calls into a shared log, so a test can assert
// the engine's ordering and its reverse-teardown-on-failure behavior.
type recPhase struct {
	id       string
	fail     bool
	optional bool
	enabled  bool
	log      *[]string
}

func (p recPhase) ID() string          { return p.id }
func (p recPhase) Title() string       { return p.id }
func (p recPhase) Optional() bool      { return p.optional }
func (p recPhase) Enabled(*State) bool { return p.enabled }
func (p recPhase) Run(context.Context, *State) error {
	*p.log = append(*p.log, "run:"+p.id)
	if p.fail {
		return errors.New("boom")
	}
	return nil
}
func (p recPhase) Teardown(context.Context, *State) error {
	*p.log = append(*p.log, "teardown:"+p.id)
	return nil
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestEngineRunsPhasesInOrder(t *testing.T) {
	var log []string
	p := func(id string) recPhase { return recPhase{id: id, enabled: true, log: &log} }
	e := &Engine{Phases: []Phase{p("a"), p("b"), p("c")}, Out: io.Discard, CleanOnFail: true}
	if err := e.Run(context.Background(), &State{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if want := []string{"run:a", "run:b", "run:c"}; !equal(log, want) {
		t.Fatalf("order = %v, want %v", log, want)
	}
}

func TestEngineTeardownReverseOnFailure(t *testing.T) {
	var log []string
	e := &Engine{
		Phases: []Phase{
			recPhase{id: "a", enabled: true, log: &log},
			recPhase{id: "b", enabled: true, log: &log},
			recPhase{id: "c", enabled: true, fail: true, log: &log},
			recPhase{id: "d", enabled: true, log: &log}, // must never run
		},
		Out:         io.Discard,
		CleanOnFail: true,
	}
	if err := e.Run(context.Background(), &State{}); err == nil {
		t.Fatal("expected error from the failing phase")
	}
	// a,b complete; c FAILS; d never runs.
	//
	// c is torn down TOO, and first. This test previously asserted the opposite —
	// that a failed phase is simply abandoned — which is the bug it was pinning: a
	// phase that fails has failed PARTWAY, so it has usually already created
	// resources. A terragrunt apply that errors on one resource has typically created
	// several others first. Rolling back only the phases that SUCCEEDED leaves exactly
	// those orphaned.
	//
	// Observed for real: cluster-addons failed on a bucket name and left seven IAM
	// roles behind, because its Teardown — which destroys the component — was never
	// called. The rollback tore down everything around it and reported success.
	want := []string{"run:a", "run:b", "run:c", "teardown:c", "teardown:b", "teardown:a"}
	if !equal(log, want) {
		t.Fatalf("log = %v, want %v", log, want)
	}
}

func TestEngineSkipsDisabledOptional(t *testing.T) {
	var log []string
	e := &Engine{
		Phases: []Phase{
			recPhase{id: "a", enabled: true, log: &log},
			recPhase{id: "opt", optional: true, enabled: false, log: &log},
			recPhase{id: "b", enabled: true, log: &log},
		},
		Out:         io.Discard,
		CleanOnFail: true,
	}
	if err := e.Run(context.Background(), &State{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if want := []string{"run:a", "run:b"}; !equal(log, want) {
		t.Fatalf("log = %v, want %v", log, want)
	}
}

func TestEngineNoTeardownWhenDisabled(t *testing.T) {
	var log []string
	e := &Engine{
		Phases: []Phase{
			recPhase{id: "a", enabled: true, log: &log},
			recPhase{id: "b", enabled: true, fail: true, log: &log},
		},
		Out:         io.Discard,
		CleanOnFail: false, // teardown suppressed
	}
	if err := e.Run(context.Background(), &State{}); err == nil {
		t.Fatal("expected error")
	}
	if want := []string{"run:a", "run:b"}; !equal(log, want) {
		t.Fatalf("log = %v, want %v (no teardown expected)", log, want)
	}
}

// A phase can fail WITHOUT the platform being torn down. The distinction is
// provisioning vs convergence: rackctl provisions the cloud; the cluster converges the
// workloads on it. If the cloud came up correctly and a workload has not settled, the
// cloud is not what is broken — and destroying it removes the only surface on which the
// problem can be diagnosed.
//
// This is not hypothetical. A fresh install provisioned cleanly, ArgoCD generated all
// 44 Applications, 42 went Healthy, and opencost was still crashlooping (it fails until
// metrics reach AMP, which takes minutes). The convergence wait expired and the engine
// destroyed a working EKS cluster — losing the evidence and forty minutes of
// provisioning, because one workload needed five more.
type norbPhase struct {
	id  string
	log *[]string
}

func (p norbPhase) ID() string          { return p.id }
func (p norbPhase) Title() string       { return p.id }
func (p norbPhase) Optional() bool      { return false }
func (p norbPhase) Enabled(*State) bool { return true }
func (p norbPhase) Run(context.Context, *State) error {
	*p.log = append(*p.log, "run:"+p.id)
	return &NoRollbackError{Err: errors.New("workloads did not converge")}
}
func (p norbPhase) Teardown(context.Context, *State) error {
	*p.log = append(*p.log, "teardown:"+p.id)
	return nil
}

func TestEngineDoesNotRollBackOnNoRollbackError(t *testing.T) {
	var log []string
	e := &Engine{
		Phases: []Phase{
			recPhase{id: "a", enabled: true, log: &log},
			norbPhase{id: "b", log: &log},
			recPhase{id: "c", enabled: true, log: &log}, // must never run
		},
		Out:         io.Discard,
		CleanOnFail: true, // rollback is ON — and must still not fire
	}
	err := e.Run(context.Background(), &State{})
	if err == nil {
		t.Fatal("expected the phase failure to surface — a convergence timeout is still a failure")
	}
	// a ran, b failed with NoRollbackError, c never ran — and NOTHING was torn down.
	want := []string{"run:a", "run:b"}
	if !equal(log, want) {
		t.Fatalf("log = %v, want %v — the platform must be left standing", log, want)
	}
}
