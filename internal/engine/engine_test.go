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
