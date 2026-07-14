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

// A failure must NEVER destroy a platform this run did not build.
//
// `init --apply` is re-runnable by design: it is how an operator retries after a failure,
// and how they re-apply a config change to a platform that is already up. Against an
// existing cluster every earlier phase "succeeds" as a NO-OP — the network is there, the
// cluster is there, nothing is created — and is recorded as completed all the same.
//
// So a failure in any later phase used to tear those phases down, and the cluster phase's
// teardown destroys the EKS cluster and the VPC. A re-apply that tripped on a config error
// would demolish a healthy, running platform.
//
// Not hypothetical: a re-apply failed on a ClusterRoleBinding conflict, the engine began
// rolling back, and the only reason a 44/44-healthy cluster survived is that the process
// happened to be killed mid-teardown.
//
// NoRollbackError guards a convergence timeout. This guards the more dangerous case: the
// operator does not lose a wait, they lose the platform.
func TestEngineNeverRollsBackAPlatformItDidNotBuild(t *testing.T) {
	orig := PlatformExists
	PlatformExists = func(context.Context, *State) bool { return true } // already provisioned
	defer func() { PlatformExists = orig }()

	var log []string
	eng := &Engine{
		Phases: []Phase{
			recPhase{id: "cluster", enabled: true, log: &log}, // a no-op against existing infra
			recPhase{id: "gitops", enabled: true, fail: true, log: &log},
		},
		Out:         io.Discard,
		CleanOnFail: true,
	}

	if err := eng.Run(context.Background(), &State{}); err == nil {
		t.Fatal("the phase failed; Run must still report the error")
	}
	for _, entry := range log {
		if len(entry) > 9 && entry[:9] == "teardown:" {
			t.Fatalf("the platform existed BEFORE this run — nothing may be torn down. "+
				"Destroying a running platform because a re-apply hit a config error is not a "+
				"rollback, it is a demolition.\ngot: %v", log)
		}
	}
}

// And the converse must still hold: when this run DID build the platform, a failure rolls
// it back — otherwise a failed fresh install strands billable resources.
func TestEngineStillRollsBackWhatItBuilt(t *testing.T) {
	orig := PlatformExists
	PlatformExists = func(context.Context, *State) bool { return false } // provisioning from zero
	defer func() { PlatformExists = orig }()

	var log []string
	eng := &Engine{
		Phases: []Phase{
			recPhase{id: "cluster", enabled: true, log: &log},
			recPhase{id: "gitops", enabled: true, fail: true, log: &log},
		},
		Out:         io.Discard,
		CleanOnFail: true,
	}
	_ = eng.Run(context.Background(), &State{})

	want := []string{"run:cluster", "run:gitops", "teardown:gitops", "teardown:cluster"}
	if len(log) != len(want) {
		t.Fatalf("a fresh install that fails must roll back what it created, or it strands "+
			"billable resources.\nwant %v\ngot  %v", want, log)
	}
	for i := range want {
		if log[i] != want[i] {
			t.Fatalf("want %v, got %v", want, log)
		}
	}
}
