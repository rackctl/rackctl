package phases

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/engine"
	"github.com/rackctl/rackctl/internal/exec"
)

// endpointState builds the minimal State clusterEndpointEnv reads: a cluster posture and a
// dry-run-aware runner. note() writes to Runner.Out, so it must be non-nil.
func endpointState(dryRun, public bool, allowlist []string) *engine.State {
	run := exec.New(io.Discard)
	run.DryRun = dryRun
	return &engine.State{
		Config: &config.Config{
			Environment: config.EnvDev,
			Cluster: config.Cluster{
				Name:                 "platform",
				EndpointPublicAccess: public,
				EndpointAllowlist:    allowlist,
			},
		},
		Runner: run,
	}
}

// withResolver swaps the egress-IP resolver for the duration of a test. The autodetect path
// must never make a real network call under test.
func withResolver(t *testing.T, f func(context.Context) (string, error)) {
	t.Helper()
	prev := egressResolver
	egressResolver = f
	t.Cleanup(func() { egressResolver = prev })
}

func hasKey(env []string, key string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, key+"=") {
			return true
		}
	}
	return false
}

const cidrsKey = "TF_VAR_cluster_endpoint_public_access_cidrs"

// A private endpoint injects the bool as false and never injects a CIDR list.
func TestClusterEndpointEnv_PrivateInjectsBoolOnly(t *testing.T) {
	withResolver(t, func(context.Context) (string, error) {
		t.Fatal("private access must not detect an egress IP")
		return "", nil
	})

	env, err := clusterEndpointEnv(context.Background(), endpointState(false, false, nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Contains(env, "TF_VAR_cluster_endpoint_public_access=false") {
		t.Errorf("private endpoint must inject public_access=false; got %v", env)
	}
	if hasKey(env, cidrsKey) {
		t.Errorf("a private endpoint has no allow-list to inject; got %v", env)
	}
}

// An explicit allow-list is injected verbatim as a JSON list — and wins over autodetection
// even at apply time (the resolver must not be consulted).
func TestClusterEndpointEnv_ExplicitAllowlistWinsOverAutodetect(t *testing.T) {
	withResolver(t, func(context.Context) (string, error) {
		t.Fatal("an explicit allow-list must win over autodetection — the resolver must not run")
		return "", nil
	})

	st := endpointState(false /* apply */, true, []string{"203.0.113.4/32", "10.0.0.0/16"})
	env, err := clusterEndpointEnv(context.Background(), st)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Contains(env, "TF_VAR_cluster_endpoint_public_access=true") {
		t.Errorf("public endpoint must inject public_access=true; got %v", env)
	}
	if !slices.Contains(env, cidrsKey+`=["203.0.113.4/32","10.0.0.0/16"]`) {
		t.Errorf("explicit allow-list must be injected as a JSON list; got %v", env)
	}
}

// Public + empty allow-list at apply time auto-detects the operator's egress IP and scopes
// the endpoint to <ip>/32 — never 0.0.0.0/0.
func TestClusterEndpointEnv_AutodetectScopesToSlash32(t *testing.T) {
	withResolver(t, func(context.Context) (string, error) { return "198.51.100.7", nil })

	env, err := clusterEndpointEnv(context.Background(), endpointState(false /* apply */, true, nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Contains(env, cidrsKey+`=["198.51.100.7/32"]`) {
		t.Errorf("autodetect must scope the endpoint to <ip>/32; got %v", env)
	}
	for _, e := range env {
		if strings.Contains(e, "0.0.0.0/0") {
			t.Errorf("autodetect must never open the endpoint to the world; got %v", env)
		}
	}
}

// A dry-run makes no network call: the resolver must not run, and no CIDR is injected (the
// value is supplied at --apply, once it is actually known). landing-zone fails closed on the
// public+empty combination by design, so a plan intentionally leaves it unset.
func TestClusterEndpointEnv_DryRunDoesNotDetectOrInjectCIDRs(t *testing.T) {
	withResolver(t, func(context.Context) (string, error) {
		t.Fatal("dry-run must not reach out to detect an egress IP")
		return "", nil
	})

	env, err := clusterEndpointEnv(context.Background(), endpointState(true /* dry-run */, true, nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Contains(env, "TF_VAR_cluster_endpoint_public_access=true") {
		t.Errorf("dry-run must still report the posture bool; got %v", env)
	}
	if hasKey(env, cidrsKey) {
		t.Errorf("dry-run must not inject a CIDR list; got %v", env)
	}
}

// A failed detection at apply time is fatal and names the fix — never a silent fall-through
// to a world-open or empty allow-list.
func TestClusterEndpointEnv_AutodetectFailureIsFatal(t *testing.T) {
	withResolver(t, func(context.Context) (string, error) {
		return "", errors.New("no route to host")
	})

	_, err := clusterEndpointEnv(context.Background(), endpointState(false /* apply */, true, nil))
	if err == nil {
		t.Fatal("a failed egress-IP detection must abort, not fall through to an unscoped endpoint")
	}
	if !strings.Contains(err.Error(), "cluster.endpointAllowlist") {
		t.Errorf("the error must name the manual escape hatch (set the allow-list); got %v", err)
	}
}
