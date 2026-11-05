package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/doctor"
	"github.com/rackctl/rackctl/internal/engine"
	rackexec "github.com/rackctl/rackctl/internal/exec"
	"github.com/rackctl/rackctl/internal/phases"
	"github.com/rackctl/rackctl/internal/reap"
	"github.com/rackctl/rackctl/internal/tui"
)

// The exit codes are a contract that docs/exit-codes.md publishes and callers branch on.
// Renumbering one silently changes what a retry loop does, so the numbers are pinned here
// rather than left to whatever the const block happens to say.
func TestExitCodes_AreStableAndDistinct(t *testing.T) {
	want := map[string]int{
		"ExitOK": 0, "ExitFailure": 1, "ExitConfig": 2, "ExitPreflight": 3,
		"ExitDeclined": 4, "ExitStanding": 5, "ExitRolledBack": 6,
		"ExitStranded": 7, "ExitAborted": 8, "ExitRefused": 9, "ExitUnhealthy": 10,
	}
	got := map[string]int{
		"ExitOK": ExitOK, "ExitFailure": ExitFailure, "ExitConfig": ExitConfig,
		"ExitPreflight": ExitPreflight, "ExitDeclined": ExitDeclined,
		"ExitStanding": ExitStanding, "ExitRolledBack": ExitRolledBack,
		"ExitStranded": ExitStranded, "ExitAborted": ExitAborted,
		"ExitRefused": ExitRefused, "ExitUnhealthy": ExitUnhealthy,
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

// The engine outcomes a caller has to act on differently must not collapse. "The rollback
// ran and there is nothing to clean up", "the platform is up and usable", "something may
// still be billing" and "nothing was touched" call for opposite next moves.
//
// Each case drives a real Engine.Run, so what is pinned is the outcome the engine records
// rather than a hand-written string that happens to match a classifier.
func TestClassifyRun_SeparatesTheCloudStates(t *testing.T) {
	boom := errors.New("boom")
	for _, tc := range []struct {
		name    string
		state   engine.PlatformState
		noClean bool
		dryRun  bool
		phases  []engine.Phase
		want    int
	}{
		{"clean", engine.PlatformAbsent, false, false,
			[]engine.Phase{outcomePhase{id: "cluster"}}, ExitOK},
		{"rolled back", engine.PlatformAbsent, false, false,
			[]engine.Phase{outcomePhase{id: "cluster"}, outcomePhase{id: "gitops", err: boom}}, ExitRolledBack},
		// The classifier used to match this text in the message. A tool that prints it to
		// stderr is not a rollback that did not complete.
		{"rolled back, stderr naming an incomplete rollback", engine.PlatformAbsent, false, false,
			[]engine.Phase{outcomePhase{id: "gitops", err: errors.New("helm: rollback did not complete")}}, ExitRolledBack},
		{"left standing, no rollback", engine.PlatformAbsent, false, false,
			[]engine.Phase{outcomePhase{id: "gitops", err: &engine.NoRollbackError{Err: errors.New("not converged")}}}, ExitStanding},
		{"left standing, optional", engine.PlatformAbsent, false, false,
			[]engine.Phase{outcomePhase{id: "cluster"}, outcomePhase{id: "portal", optional: true, err: boom}}, ExitStanding},
		{"left standing, preexisting", engine.PlatformPresent, false, false,
			[]engine.Phase{outcomePhase{id: "gitops", err: boom}}, ExitStanding},
		{"platform state unknown", engine.PlatformUnknown, false, false,
			[]engine.Phase{outcomePhase{id: "gitops", err: boom}}, ExitStranded},
		{"--no-clean-on-failure", engine.PlatformAbsent, true, false,
			[]engine.Phase{outcomePhase{id: "cluster"}, outcomePhase{id: "gitops", err: boom}}, ExitStranded},
		{"stranded", engine.PlatformAbsent, false, false,
			[]engine.Phase{outcomePhase{id: "cluster", teardownErr: errors.New("DependencyViolation")},
				outcomePhase{id: "substrate", err: boom}}, ExitStranded},
		{"refused before spend", engine.PlatformAbsent, false, false,
			[]engine.Phase{outcomePhase{id: "acquire", err: engine.Refuse(errors.New("no ref v9"))}}, ExitRefused},
		// A refusal touched nothing, whatever the run found standing or could not find out.
		{"refused before spend, over a standing platform", engine.PlatformPresent, false, false,
			[]engine.Phase{outcomePhase{id: "acquire", err: engine.Refuse(errors.New("no ref v9"))}}, ExitRefused},
		{"refused before spend, platform state unknown", engine.PlatformUnknown, false, false,
			[]engine.Phase{outcomePhase{id: "acquire", err: engine.Refuse(errors.New("no ref v9"))}}, ExitRefused},
		{"a failed plan", engine.PlatformAbsent, false, true,
			[]engine.Phase{outcomePhase{id: "cluster"}, outcomePhase{id: "gitops", err: boom}}, ExitFailure},
		{"a failed plan whose rollback would not complete", engine.PlatformAbsent, false, true,
			[]engine.Phase{outcomePhase{id: "cluster", teardownErr: errors.New("x")}, outcomePhase{id: "gitops", err: boom}}, ExitFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := runEngine(t, context.Background(), tc.state, tc.noClean, tc.dryRun, tc.phases...)
			if got := ExitCode(classifyRun(context.Background(), err)); got != tc.want {
				t.Errorf("exit %d, want %d (%v)", got, tc.want, err)
			}
		})
	}
}

// A plain-path interrupt cancels the command context, and the run unwinds. It is aborted
// when that unwind finished, and resources-may-remain when it did not: what may still be
// billing is the more urgent fact. Either way the message says it was interrupted.
func TestClassifyRun_AnInterruptIsAbortedUnlessTheUnwindDidNotFinish(t *testing.T) {
	for _, tc := range []struct {
		name        string
		teardownErr error
		want        int
	}{
		{"unwind finished", nil, ExitAborted},
		{"unwind did not finish", errors.New("DependencyViolation"), ExitStranded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			err := runEngine(t, ctx, engine.PlatformAbsent, false, false,
				outcomePhase{id: "cluster", teardownErr: tc.teardownErr},
				outcomePhase{id: "substrate", err: errors.New("terragrunt: signal: interrupt"), cancel: cancel})
			classified := classifyRun(ctx, err)
			if got := ExitCode(classified); got != tc.want {
				t.Errorf("exit %d, want %d (%v)", got, tc.want, classified)
			}
			if !strings.HasPrefix(classified.Error(), "interrupted: ") {
				t.Errorf("the message must say the run was interrupted: %v", classified)
			}
			// The engine's own record carries it too, for a caller whose context is not the
			// one that was cancelled — the TUI cancels a context of its own.
			if got := ExitCode(classifyRun(context.Background(), err)); got != tc.want {
				t.Errorf("from the engine's record alone: exit %d, want %d (%v)", got, tc.want, err)
			}
		})
	}

	// A cancelled command context with an error that carries no engine outcome, the TUI's
	// quit, and an error that carries no outcome and no interrupt at all.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if got := ExitCode(classifyRun(cancelled, errors.New("tui: no terminal"))); got != ExitAborted {
		t.Errorf("an interrupted command: exit %d, want %d", got, ExitAborted)
	}
	if got := ExitCode(classifyRun(context.Background(), fmt.Errorf("run: %w", tui.ErrAborted))); got != ExitAborted {
		t.Errorf("a TUI abort: exit %d, want %d", got, ExitAborted)
	}
	if got := ExitCode(classifyRun(context.Background(), errors.New("tui: no terminal"))); got != ExitFailure {
		t.Errorf("an error with no recorded outcome is not evidence of a rollback: exit %d, want %d", got, ExitFailure)
	}
}

// An interrupt that lands while the run is asking EKS whether a platform exists makes that
// question fail, so the platform state is unknown, and the preflight phase then refuses.
// Nothing was touched, so the run is aborted, not resources-may-remain.
func TestClassifyRun_AnInterruptDuringPlatformDetectionIsAborted(t *testing.T) {
	orig := engine.PlatformExists
	t.Cleanup(func() { engine.PlatformExists = orig })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var state engine.PlatformState
	engine.PlatformExists = func(ctx context.Context, st *engine.State) engine.PlatformState {
		cancel()
		state = orig(ctx, st)
		return state
	}
	t.Setenv("PATH", t.TempDir())
	eng := &engine.Engine{Phases: phases.All()[:1], Out: io.Discard, CleanOnFail: true}
	err := eng.Run(ctx, &engine.State{Runner: rackexec.New(io.Discard), Config: &config.Config{}})
	if state != engine.PlatformUnknown {
		t.Fatalf("the interrupted detection must read as unknown, got %v", state)
	}
	classified := classifyRun(ctx, err)
	if got := ExitCode(classified); got != ExitAborted {
		t.Errorf("exit %d, want %d (%v)", got, ExitAborted, classified)
	}
	if !strings.HasPrefix(classified.Error(), "interrupted: ") {
		t.Errorf("the message must say the run was interrupted: %v", classified)
	}
}

// runEngine runs phases through a real Engine against the production Runner, with the
// platform-exists answer fixed. No phase here shells out, and no cluster was pointed at,
// so the rollback's sweeps have nothing to act on.
func runEngine(t *testing.T, ctx context.Context, state engine.PlatformState, noClean, dryRun bool, phases ...engine.Phase) error {
	t.Helper()
	orig := engine.PlatformExists
	t.Cleanup(func() { engine.PlatformExists = orig })
	engine.PlatformExists = func(context.Context, *engine.State) engine.PlatformState { return state }
	t.Setenv("PATH", t.TempDir())
	run := rackexec.New(io.Discard)
	run.DryRun = dryRun
	eng := &engine.Engine{Phases: phases, Out: io.Discard, CleanOnFail: !noClean}
	return eng.Run(ctx, &engine.State{Runner: run, Config: &config.Config{}})
}

// outcomePhase fails with err when it is set, and its teardown fails with teardownErr.
// cancel, when set, is called from inside Run: the interrupt arriving mid-phase.
type outcomePhase struct {
	id          string
	optional    bool
	err         error
	teardownErr error
	cancel      context.CancelFunc
}

func (p outcomePhase) ID() string                 { return p.id }
func (p outcomePhase) Title() string              { return p.id }
func (p outcomePhase) Optional() bool             { return p.optional }
func (p outcomePhase) Enabled(*engine.State) bool { return true }
func (p outcomePhase) Run(context.Context, *engine.State) error {
	if p.cancel != nil {
		p.cancel()
	}
	return p.err
}
func (p outcomePhase) Teardown(context.Context, *engine.State) error { return p.teardownErr }

// A rollback whose guard did not hold leaves something that may be billing, so the engine's
// outcome classifies as resources-may-remain, not rolled-back. An informational report that
// did not run decides nothing.
func TestClassifyRun_ARollbackWhoseGuardDidNotHoldIsStranded(t *testing.T) {
	for _, tc := range []struct {
		b    reap.Backstop
		want int
	}{
		{reap.Backstop{Name: "Karpenter nodes", Outcome: reap.Failed}, ExitStranded},
		{reap.Backstop{Name: "orphaned EBS volumes", Outcome: reap.NotRun}, ExitStranded},
		{reap.Backstop{Name: "unprovable EBS volumes", Outcome: reap.NotRun, Informational: true}, ExitRolledBack},
		{reap.Backstop{Name: "orphaned EBS volumes", Outcome: reap.FoundNothing}, ExitRolledBack},
	} {
		err := runEngine(t, context.Background(), engine.PlatformAbsent, false, false,
			backstopPhase{id: "cluster", b: tc.b}, backstopPhase{id: "gitops", fail: true})
		if got := ExitCode(classifyRun(context.Background(), err)); got != tc.want {
			t.Errorf("%s %s: exit %d, want %d (%v)", tc.b.Name, tc.b.Outcome, got, tc.want, err)
		}
	}
}

// backstopPhase records its backstop on teardown, the way the cluster phase records the
// volume sweep.
type backstopPhase struct {
	id   string
	fail bool
	b    reap.Backstop
}

func (p backstopPhase) ID() string                 { return p.id }
func (p backstopPhase) Title() string              { return p.id }
func (p backstopPhase) Optional() bool             { return false }
func (p backstopPhase) Enabled(*engine.State) bool { return true }
func (p backstopPhase) Run(context.Context, *engine.State) error {
	if p.fail {
		return errors.New("boom")
	}
	return nil
}
func (p backstopPhase) Teardown(_ context.Context, st *engine.State) error {
	if p.b.Name != "" {
		st.Backstops = append(st.Backstops, p.b)
	}
	return nil
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
		{withExit(ExitRefused, errors.New("x")), "refused (exit 9)"},
		{withExit(ExitUnhealthy, errors.New("x")), "unhealthy (exit 10)"},
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
	report := newCheckReport(cfg, "asserted",
		[]doctor.Result{{Name: "vcpu quota", Status: doctor.Fail, Detail: "32 vCPU"}},
		[]doctor.Result{{Name: "applications", Status: doctor.OK, Detail: "44 Healthy"}},
		true)
	report.ClusterExists = "present"
	emitErr := writeCheckReport(report)
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
		"clusterHealth", "passed", "preflight", "platform", "clusterExists"} {
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
