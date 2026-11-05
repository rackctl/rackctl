package cmd

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/config"
	rackexec "github.com/rackctl/rackctl/internal/exec"
)

// guard runs spokeGuard against a kubectl on PATH that runs body.
func guard(t *testing.T, body string, eksFleet, dryRun, reaping, clusterGone bool) error {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg := &config.Config{Environment: config.EnvDev}
	cfg.Cluster.Name = "platform"
	cfg.ControlPlane.EKSFleet = eksFleet
	run := rackexec.New(io.Discard)
	run.DryRun, run.ReadAttempts = dryRun, 1
	return spokeGuard(context.Background(), run, cfg, reaping, clusterGone, errors.New("AccessDenied"))
}

const (
	spokes      = "echo 'fleet/spoke-a'\n"
	noHub       = "echo 'error: the server doesn'\"'\"'t have a resource type \"clusters\"' >&2\nexit 1\n"
	unreachable = "echo 'Unable to connect to the server: dial tcp 10.0.0.1:443: i/o timeout' >&2\nexit 1\n"
)

// With controlPlane.eksFleet on, a spoke set that cannot be read is a refusal, with no
// override: an RBAC denial, a timeout or an unreachable hub all used to read as "no spokes",
// the one answer that lets the destroy go ahead and strand clusters that bill forever.
func TestSpokeGuard_AnUnknownSpokeSetRefusesWhenEKSFleetIsOn(t *testing.T) {
	err := guard(t, unreachable, true, false, true, false)
	if err == nil || !strings.Contains(err.Error(), "could not be listed") || !strings.Contains(err.Error(), "Nothing was destroyed") {
		t.Fatalf("an unreachable hub must refuse: %v", err)
	}
	err = guard(t, spokes, true, false, false, false)
	if err == nil || !strings.Contains(err.Error(), "kubectl could not be pointed at development-platform") {
		t.Fatalf("a hub kubectl could not be pointed at must refuse: %v", err)
	}
}

// The cases that may go on: no hub, a cluster already gone, an empty spoke set, and an
// unreadable answer where eksFleet is off.
func TestSpokeGuard_WhatMayGoOn(t *testing.T) {
	for _, c := range []struct {
		name                           string
		body                           string
		eksFleet, reaping, clusterGone bool
	}{
		{"no hub", noHub, true, true, false},
		{"no spokes", "exit 0\n", true, true, false},
		{"the cluster is gone", unreachable, true, false, true},
		{"eksFleet off and the repoint failed", unreachable, false, false, false},
		{"eksFleet off and unreadable", unreachable, false, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := guard(t, c.body, c.eksFleet, false, c.reaping, c.clusterGone); err != nil {
				t.Errorf("got %v", err)
			}
		})
	}
}

// Live spokes refuse, and the message says what changed: the operator's kubeconfig on a real
// run, nothing at all on a dry-run, which reads through a temporary one.
func TestSpokeGuard_LiveSpokesRefuseAndSayWhatChanged(t *testing.T) {
	err := guard(t, spokes, false, false, true, false)
	if err == nil || !strings.Contains(err.Error(), "fleet/spoke-a") || !strings.Contains(err.Error(), "now points at development-platform") {
		t.Fatalf("got %v", err)
	}
	err = guard(t, spokes, false, true, true, false)
	if err == nil || !strings.Contains(err.Error(), "your kubeconfig is untouched") {
		t.Fatalf("a dry-run refusal must not claim it repointed kubectl: %v", err)
	}
}
