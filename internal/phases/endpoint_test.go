package phases

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
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

// withEKS puts a fake aws on PATH whose describe-cluster runs body. noCluster is the
// answer for a cluster that does not exist yet.
func withEKS(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "aws"), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

const noCluster = "echo 'An error occurred (ResourceNotFoundException) when calling the DescribeCluster operation' >&2\nexit 254\n"

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

	env, err := clusterEndpointEnv(context.Background(), endpointState(false, false, nil), "apply")
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
	env, err := clusterEndpointEnv(context.Background(), st, "apply")
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
	withEKS(t, noCluster)
	withResolver(t, func(context.Context) (string, error) { return "198.51.100.7", nil })

	env, err := clusterEndpointEnv(context.Background(), endpointState(false /* apply */, true, nil), "apply")
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
	withEKS(t, noCluster)
	withResolver(t, func(context.Context) (string, error) {
		t.Fatal("dry-run must not reach out to detect an egress IP")
		return "", nil
	})

	env, err := clusterEndpointEnv(context.Background(), endpointState(true /* dry-run */, true, nil), "apply")
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
	withEKS(t, noCluster)
	withResolver(t, func(context.Context) (string, error) {
		return "", errors.New("no route to host")
	})

	_, err := clusterEndpointEnv(context.Background(), endpointState(false /* apply */, true, nil), "apply")
	if err == nil {
		t.Fatal("a failed egress-IP detection must abort, not fall through to an unscoped endpoint")
	}
	if !strings.Contains(err.Error(), "cluster.endpointAllowlist") {
		t.Errorf("the error must name the manual escape hatch (set the allow-list); got %v", err)
	}
}

// A re-apply keeps the allow-list the cluster already has. Re-detecting it from another host
// re-scoped the endpoint to that host and locked out the operator who built it; changing the
// list takes an explicit cluster.endpointAllowlist.
func TestClusterEndpointEnv_AReapplyKeepsTheLiveAllowlist(t *testing.T) {
	withEKS(t, `echo '{"public":true,"cidrs":["203.0.113.9/32"]}'`+"\n")
	withResolver(t, func(context.Context) (string, error) { return "198.51.100.7", nil })
	var out strings.Builder
	st := endpointState(false /* apply */, true, nil)
	st.Runner.Out = &out

	env, err := clusterEndpointEnv(context.Background(), st, "apply")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Contains(env, cidrsKey+`=["203.0.113.9/32"]`) {
		t.Errorf("the live allow-list must be kept on a re-apply; got %v", env)
	}
	if !strings.Contains(out.String(), "198.51.100.7 is NOT in it") {
		t.Errorf("a host outside the kept list must be told so:\n%s", out.String())
	}
}

// A live allow-list that cannot be read is not a first install: detecting over it could
// lock out whoever it admits, so the apply refuses and names the explicit field.
func TestClusterEndpointEnv_AnUnreadableLiveAllowlistRefuses(t *testing.T) {
	withEKS(t, "echo 'An error occurred (AccessDeniedException)' >&2\nexit 254\n")
	withResolver(t, func(context.Context) (string, error) {
		t.Fatal("nothing may be detected over a cluster whose allow-list is unknown")
		return "", nil
	})
	st := endpointState(false, true, nil)
	st.Runner.ReadAttempts = 1
	_, err := clusterEndpointEnv(context.Background(), st, "apply")
	if err == nil || !strings.Contains(err.Error(), "cluster.endpointAllowlist") {
		t.Fatalf("want a refusal naming cluster.endpointAllowlist, got %v", err)
	}
}

// A destroy never detects an egress IP. It passes the live list when it can read it and a
// placeholder that admits nobody when it cannot, so the destroy plan's precondition holds.
func TestClusterEndpointEnv_ADestroyNeverDetects(t *testing.T) {
	withResolver(t, func(context.Context) (string, error) {
		t.Fatal("a destroy must not detect an egress IP")
		return "", nil
	})
	for _, c := range []struct{ name, aws, want string }{
		{"live list", `echo '{"public":true,"cidrs":["203.0.113.9/32"]}'` + "\n", `["203.0.113.9/32"]`},
		{"cluster gone", noCluster, `["` + destroyPlaceholderCIDR + `"]`},
		{"unreadable", "echo 'An error occurred (AccessDeniedException)' >&2\nexit 254\n", `["` + destroyPlaceholderCIDR + `"]`},
	} {
		t.Run(c.name, func(t *testing.T) {
			withEKS(t, c.aws)
			var out strings.Builder
			st := endpointState(false, true, nil)
			st.Runner.Out, st.Runner.ReadAttempts = &out, 1
			env, err := clusterEndpointEnv(context.Background(), st, "destroy")
			if err != nil {
				t.Fatalf("a destroy must not fail on the allow-list: %v", err)
			}
			if !slices.Contains(env, cidrsKey+"="+c.want) {
				t.Errorf("got %v, want %s", env, c.want)
			}
			if !strings.Contains(out.String(), "(destroy)") || strings.Contains(out.String(), "auto-detected") {
				t.Errorf("the note must use destroy wording:\n%s", out.String())
			}
		})
	}
}

// The verb reaches the endpoint builder through componentEnv, which is how both destroy
// paths and the bucket permit build the cluster component's env.
func TestComponentEnv_TheClusterDestroyCarriesTheVerb(t *testing.T) {
	withEKS(t, noCluster)
	withResolver(t, func(context.Context) (string, error) {
		t.Fatal("a destroy must not detect an egress IP")
		return "", nil
	})
	st := endpointState(false, true, nil)
	if _, err := componentEnv(context.Background(), st, "cluster", "destroy"); err != nil {
		t.Fatalf("componentEnv: %v", err)
	}
}

// EKS reports 0.0.0.0/0 as the allow-list of a cluster whose public endpoint is off. Turning
// public access on over such a cluster must scope the endpoint to this host, not keep a list
// that would open the API to the whole internet.
func TestClusterEndpointEnv_APrivateClusterReportingTheWorldIsNotKept(t *testing.T) {
	withEKS(t, `echo '{"public":false,"cidrs":["0.0.0.0/0"]}'`+"\n")
	withResolver(t, func(context.Context) (string, error) { return "198.51.100.7", nil })
	var out strings.Builder
	st := endpointState(false /* apply */, true, nil)
	st.Runner.Out = &out

	env, err := clusterEndpointEnv(context.Background(), st, "apply")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Contains(env, cidrsKey+`=["198.51.100.7/32"]`) {
		t.Errorf("a private cluster has no allow-list to keep; the endpoint must be scoped to <ip>/32, got %v", env)
	}
	for _, e := range env {
		if strings.Contains(e, "0.0.0.0/0") {
			t.Errorf("the default allow-list of a private cluster must never be injected; got %v", env)
		}
	}
	if !strings.Contains(out.String(), "public endpoint off") {
		t.Errorf("the note must say why nothing was kept:\n%s", out.String())
	}
}

// A destroy over a private cluster passes the placeholder, never the default 0.0.0.0/0.
func TestClusterEndpointEnv_ADestroyOverAPrivateClusterPassesThePlaceholder(t *testing.T) {
	withEKS(t, `echo '{"public":false,"cidrs":["0.0.0.0/0"]}'`+"\n")
	withResolver(t, func(context.Context) (string, error) {
		t.Fatal("a destroy must not detect an egress IP")
		return "", nil
	})
	var out strings.Builder
	st := endpointState(false, true, nil)
	st.Runner.Out = &out
	env, err := clusterEndpointEnv(context.Background(), st, "destroy")
	if err != nil {
		t.Fatalf("a destroy must not fail on the allow-list: %v", err)
	}
	if !slices.Contains(env, cidrsKey+`=["`+destroyPlaceholderCIDR+`"]`) {
		t.Errorf("got %v, want the placeholder", env)
	}
	if !strings.Contains(out.String(), "public endpoint is off") {
		t.Errorf("the note must say the endpoint is private:\n%s", out.String())
	}
}

// A live public list that admits every address is never kept implicitly, however the
// zero-length prefix is spelled; opening the API to the world takes an explicit list.
func TestClusterEndpointEnv_AWorldOpenLiveListRefuses(t *testing.T) {
	for _, world := range []string{"0.0.0.0/0", "::/0", "10.0.0.0/0"} {
		t.Run(world, func(t *testing.T) {
			withEKS(t, `echo '{"public":true,"cidrs":["203.0.113.9/32","`+world+`"]}'`+"\n")
			withResolver(t, func(context.Context) (string, error) {
				t.Fatal("nothing may be detected over a world-open live list")
				return "", nil
			})
			_, err := clusterEndpointEnv(context.Background(), endpointState(false, true, nil), "apply")
			if err == nil || !strings.Contains(err.Error(), "cluster.endpointAllowlist") ||
				!strings.Contains(err.Error(), world) {
				t.Fatalf("want a refusal naming %s and cluster.endpointAllowlist, got %v", world, err)
			}
		})
	}
}

// A live public endpoint with no allow-list, or a posture that does not say whether the
// endpoint is public, refuses rather than detecting over it.
func TestClusterEndpointEnv_AnIncompleteLivePostureRefuses(t *testing.T) {
	for name, body := range map[string]string{
		"public, no list": `echo '{"public":true,"cidrs":[]}'`,
		"no public field": `echo '{"cidrs":["203.0.113.9/32"]}'`,
		"not an object":   `echo '["203.0.113.9/32"]'`,
	} {
		t.Run(name, func(t *testing.T) {
			withEKS(t, body+"\n")
			withResolver(t, func(context.Context) (string, error) {
				t.Fatal("nothing may be detected over a cluster whose posture is incomplete")
				return "", nil
			})
			_, err := clusterEndpointEnv(context.Background(), endpointState(false, true, nil), "apply")
			if err == nil || !strings.Contains(err.Error(), "cluster.endpointAllowlist") {
				t.Fatalf("want a refusal naming cluster.endpointAllowlist, got %v", err)
			}
		})
	}
}
