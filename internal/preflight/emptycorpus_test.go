package preflight

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/doctor"
	"github.com/rackctl/rackctl/internal/exec"
)

// silentEnv puts an `aws` on PATH that exits 0 and prints nothing.
//
// That pairing is what a check cannot afford to read as good news: it is the shape of a
// --query path that matches nothing, a shimmed or wrapped binary, and a response whose
// field the query names is absent. A gate whose verdict is drawn from an absence cannot
// tell any of those from a healthy account, so each check below is pinned against the
// silent case rather than against a populated one.
func silentEnv(t *testing.T) *Env {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "aws"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write silent aws: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := config.Default()
	cfg.Cloud.AccountID = "111111111111"
	cfg.Cloud.Profile = "test"
	cfg.Cluster.Name = "platform"
	return &Env{Cfg: cfg, Run: exec.New(io.Discard)}
}

// A zero exit says the command ran. It does not say a cluster answered, and the two must
// not collapse: reading the first as the second returns "is live" and skips every
// collision the check exists to find.
func TestCollisionsRequiresAClusterStatus(t *testing.T) {
	res := CheckCollisions(context.Background(), silentEnv(t))

	if strings.Contains(res.Detail, "is live") {
		t.Errorf("silent aws was read as a live cluster, skipping every collision check: %q", res.Detail)
	}
}

// An empty InvalidParameters is the absence of a negative. The contract is published only
// when the parameters are counted and all of them resolve.
func TestCostPipelineCountsResolvedParameters(t *testing.T) {
	// Enable and CostPipeline both default to on when omitted, so the check runs its
	// body rather than short-circuiting on a disabled tier.
	res := CheckCostPipelinePrerequisite(context.Background(), silentEnv(t))

	if res.Status == doctor.OK {
		t.Errorf("silent aws published the CUR export contract on an empty invalid-name list: %q", res.Detail)
	}
}
