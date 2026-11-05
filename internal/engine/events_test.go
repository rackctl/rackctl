package engine

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/exec"
)

// A plan shows whether a failed apply would roll back. It asks EKS for real, through the
// read that runs in a dry-run: a plan that assumed a fresh install would hide the existing
// platform an apply would refuse to roll back, and the fail-closed warning with it.
func TestPlan_SaysAnApplyWouldNotRollBackAnExistingPlatform(t *testing.T) {
	for _, c := range []struct {
		name, aws, want string
	}{
		{"present", "echo True\n", "existing platform detected — plan only: apply would NOT roll back"},
		{"unknown", "echo 'An error occurred (ExpiredToken)' >&2\nexit 254\n",
			"could not determine whether a platform already exists — plan only: apply would NOT roll back"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "aws"), []byte("#!/bin/sh\n"+c.aws), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			var out bytes.Buffer
			run := exec.New(&out)
			run.DryRun = true
			run.ReadAttempts = 1
			eng := &Engine{Phases: []Phase{recPhase{id: "cluster", enabled: true, fail: true, log: new([]string)}},
				Out: &out, CleanOnFail: true}
			_ = eng.Run(context.Background(), &State{Runner: run, Config: &config.Config{}})
			if !strings.Contains(out.String(), c.want) {
				t.Errorf("missing %q:\n%s", c.want, out.String())
			}
			if strings.Contains(out.String(), "would roll back provisioned resources") {
				t.Errorf("a plan over a platform an apply would not roll back printed a rollback walk:\n%s", out.String())
			}
		})
	}
}

// tdPhase is a phase whose teardown can fail.
type tdPhase struct {
	recPhase
	teardownErr error
}

func (p tdPhase) Teardown(context.Context, *State) error { return p.teardownErr }

// Under a Hook the posture, the rollback and every teardown still reach Out, which is the
// transcript under the TUI, and the Hook is told each one: the rollback as a notice, each
// phase it undid or could not undo against that phase's row.
func TestRun_UnderAHookThePostureAndTheTeardownAreRecorded(t *testing.T) {
	assumePlatform(t, PlatformPresent)
	var out bytes.Buffer
	var evs []Event
	eng := &Engine{Phases: []Phase{recPhase{id: "x", enabled: true, fail: true, log: new([]string)}},
		Out: &out, CleanOnFail: true, Hook: func(ev Event) { evs = append(evs, ev) }}
	_ = eng.Run(context.Background(), &State{Runner: exec.New(&out), Config: &config.Config{}})
	if !strings.Contains(out.String(), "existing platform detected — a failure will NOT roll it back") ||
		!strings.Contains(out.String(), "the platform was already provisioned before this run") {
		t.Errorf("the posture and the leave-standing line must reach Out under a Hook:\n%s", out.String())
	}
	if len(evs) == 0 || !evs[0].Posture || evs[0].Index != 0 {
		t.Errorf("the Hook must be told the posture first, as a notice: %+v", evs)
	}

	assumePlatform(t, PlatformAbsent)
	out.Reset()
	evs = nil
	eng = &Engine{Phases: []Phase{
		tdPhase{recPhase: recPhase{id: "network", enabled: true, log: new([]string)}},
		tdPhase{recPhase: recPhase{id: "cluster", enabled: true, log: new([]string)}, teardownErr: errors.New("DependencyViolation")},
		recPhase{id: "gitops", enabled: true, fail: true, log: new([]string)},
	}, Out: &out, CleanOnFail: true, Hook: func(ev Event) { evs = append(evs, ev) }}
	_ = eng.Run(context.Background(), &State{Runner: exec.New(&out), Config: &config.Config{}})
	for _, want := range []string{"failure detected — rolling back provisioned resources",
		"teardown: network", "torn down: network", "teardown cluster: DependencyViolation"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q from Out under a Hook:\n%s", want, out.String())
		}
	}
	got := map[int]Status{}
	rollback := false
	for _, ev := range evs {
		if ev.Rollback {
			rollback = true
		}
		if rollback && ev.Index > 0 {
			got[ev.Index] = ev.Status
		}
	}
	if !rollback {
		t.Fatalf("the Hook was never told the rollback began: %+v", evs)
	}
	if got[1] != StatusTornDown || got[2] != StatusTeardownFail || got[3] != StatusTornDown {
		t.Errorf("each undone phase must be reported against its own row: %v", got)
	}
}

// The closing line names the portal only when this config installs one.
func TestSuccessLine_NamesThePortalOnlyWhenOneIsInstalled(t *testing.T) {
	if got := SuccessLine(&config.Config{}); strings.Contains(got, "portal") {
		t.Errorf("no portal is installed, and the line hands off to one: %q", got)
	}
	cfg := &config.Config{}
	cfg.ControlPlane.Portal = true
	if got := SuccessLine(cfg); !strings.Contains(got, "hand off to the portal") {
		t.Errorf("a portal is installed, and the line does not name it: %q", got)
	}
}
