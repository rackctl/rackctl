package phases

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/engine"
	"github.com/rackctl/rackctl/internal/exec"
)

// fakeAWSAndTG puts an `aws` and a `terragrunt` on PATH. `aws eks update-kubeconfig` exits
// with updateExit; everything else succeeds, so cluster.Run reaches the repoint.
func fakeAWSAndTG(t *testing.T, updateExit int) *engine.State {
	t.Helper()
	dir := t.TempDir()
	aws := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "eks" ] && [ "$2" = "update-kubeconfig" ]; then exit %d; fi
exit 0
`, updateExit)
	if err := os.WriteFile(filepath.Join(dir, "aws"), []byte(aws), 0o755); err != nil {
		t.Fatalf("write fake aws: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "terragrunt"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake terragrunt: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := baseCfg()
	cfg.Environment = config.EnvDev
	cfg.Cluster.Name = "platform"
	cfg.Cluster.Network = config.ClusterNet{VPCCIDR: "10.0.0.0/16", NATGateways: 1}
	// A real directory, because tg() chdirs into the checkout before invoking terragrunt.
	return &engine.State{
		Config: cfg,
		Runner: exec.New(io.Discard),
		Repos:  engine.Repos{Workdir: dir, LandingZone: dir},
	}
}

// engine.State.KubeconfigCluster is the sole gate on the rollback's ambient-context reap
// sweep, and phases.go is its only producer. Neither direction had a test: deleting the
// assignment, or hoisting it ABOVE the update-kubeconfig call, both left the whole suite green
// — so a regression either strands billable EBS volumes and IAM roles, or fires
// `kubectl delete platforms|tenants|nodeclaims|pvc --all -A` at the operator's previous
// context. Both are silent.
func TestClusterPhase_RecordsTheKubeconfigItRepointed(t *testing.T) {
	st := fakeAWSAndTG(t, 0)

	if err := (cluster{}).Run(context.Background(), st); err != nil {
		t.Fatalf("cluster.Run: %v", err)
	}
	if got, want := st.KubeconfigCluster, st.Config.ClusterName(); got != want {
		t.Fatalf("KubeconfigCluster = %q, want %q. The rollback's reap sweep is gated on this "+
			"field, so an unset one means a failed install strands whatever the controllers own — "+
			"unattached EBS volumes and operator-minted IAM roles nothing will collect.", got, want)
	}
}

// And the ordering is the point, not an incidental. Recorded AFTER the command, so a FAILED
// repoint leaves the sweep disabled: kubectl still resolves whatever context the operator was
// on, and the sweep deletes everything it can see there.
func TestClusterPhase_DoesNotClaimAKubeconfigItFailedToRepoint(t *testing.T) {
	st := fakeAWSAndTG(t, 1) // update-kubeconfig fails

	if err := (cluster{}).Run(context.Background(), st); err == nil {
		t.Fatal("a failed update-kubeconfig must fail the phase")
	}
	if st.KubeconfigCluster != "" {
		t.Fatalf("KubeconfigCluster = %q after a FAILED repoint. kubectl still points at the "+
			"operator's previous context, so claiming the fact here hands the rollback permission "+
			"to delete every Platform and PVC in an unrelated cluster.", st.KubeconfigCluster)
	}
}
