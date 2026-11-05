package phases

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/rackctl/rackctl/internal/engine"
	"github.com/rackctl/rackctl/internal/exec"
)

// platformRun runs the platform phase for real against fake terragrunt, aws and kubectl
// bodies, with every live root it asserts in place.
func platformRun(t *testing.T, terragrunt, kubectl string) error {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{"terragrunt": terragrunt, "kubectl": kubectl, "aws": "exit 0\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	fixtureSt, _ := apState(t)
	for _, c := range allAPComponents() {
		root := filepath.Join(dir, agentPlatformDir(fixtureSt, c))
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "terragrunt.hcl"), []byte("# fixture\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	st, _ := apState(t)
	st.Runner = exec.New(io.Discard)
	st.Repos = engine.Repos{Workdir: dir, LandingZone: dir, AgentPlatform: dir}
	return (platform{}).Run(context.Background(), st)
}

// Once the agent-platform tree is built, the phase is converging rather than provisioning,
// and a failure to converge must not destroy the cloud: the CRD and operator waits and the
// eval-runtime republish leave the platform standing. A failure to apply the tree itself is
// a failure to provision, and still rolls back.
func TestPlatform_AConvergenceFailureLeavesThePlatformStanding(t *testing.T) {
	var noRollback *engine.NoRollbackError
	for _, c := range []struct{ name, terragrunt, kubectl string }{
		{"a CRD that never establishes", "exit 0\n", `case "$*" in *Established*) exit 1 ;; esac` + "\nexit 0\n"},
		{"an operator that never becomes Available", "exit 0\n", `case "$*" in *Available*) exit 1 ;; esac` + "\nexit 0\n"},
		{"the eval-runtime republish", `case "$*" in *cluster-bootstrap*apply*) exit 1 ;; esac` + "\nexit 0\n", "exit 0\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := platformRun(t, c.terragrunt, c.kubectl)
			if err == nil {
				t.Fatal("the failure must fail the phase")
			}
			if !errors.As(err, &noRollback) || noRollback.BeforeSpend {
				t.Fatalf("a convergence failure rolled the cloud back: %v", err)
			}
		})
	}
	t.Run("an agent-platform apply", func(t *testing.T) {
		err := platformRun(t, `case "$*" in *cluster-bootstrap*) exit 0 ;; *apply*) exit 1 ;; esac`+"\nexit 0\n", "exit 0\n")
		if err == nil {
			t.Fatal("the failure must fail the phase")
		}
		if errors.As(err, &noRollback) {
			t.Fatalf("a failure to provision must still roll back: %v", err)
		}
	})
}
