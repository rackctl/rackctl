package phases

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/rackctl/rackctl/internal/engine"
)

// ipEchoEndpoint is the boring, well-known IP-echo service used to discover the operator's
// public egress IP. It returns the caller's source address as plain text and nothing else —
// no query string, no auth, no JSON to misparse.
const ipEchoEndpoint = "https://checkip.amazonaws.com"

// egressResolver fetches the operator's public egress IP. It is a package var so the
// autodetect path can be exercised in tests without a real network call.
var egressResolver = fetchEgressIP

// EgressIP resolves this host's public egress IP through the same resolver the endpoint
// allow-list uses, so a report of an unreachable cluster names the address that allow-list
// would have to hold.
func EgressIP(ctx context.Context) (string, error) { return egressResolver(ctx) }

// fetchEgressIP asks ipEchoEndpoint for this host's public source address, with a bounded
// timeout so a hung resolver cannot stall a provision. It returns the bare IP (no mask).
func fetchEgressIP(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ipEchoEndpoint, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	// Discarded explicitly. Closing a response body this function has finished reading can only
	// fail on an already-broken connection, and there is nothing to do about it and nothing to
	// report it to — the caller wants an IP or an error about getting one.
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s returned %s", ipEchoEndpoint, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return "", err
	}
	ip := strings.TrimSpace(string(body))
	if net.ParseIP(ip) == nil {
		return "", fmt.Errorf("%s returned %q, which is not an IP", ipEchoEndpoint, ip)
	}
	return ip, nil
}

// destroyPlaceholderCIDR is what a destroy passes for the allow-list when the live one
// cannot be read. It is a documentation address (RFC 5737) that admits nobody: a destroy
// changes no allow-list, and the value exists only so the cluster component's fail-closed
// precondition, a public endpoint with a non-empty list, holds while it plans.
const destroyPlaceholderCIDR = "192.0.2.0/32"

// clusterEndpointEnv builds the TF_VAR_* entries that carry the operator's chosen EKS
// API-endpoint posture into landing-zone's cluster component, whose committed tree is
// private-by-default and fail-closed. It returns the env to append to the terragrunt runner
// after printing operator-facing notes about exactly what lands on the control plane — the
// CIDR the operator sees here is the CIDR the API server accepts.
//
// Posture:
//   - endpointPublicAccess=false            -> private endpoint; no CIDR list injected.
//   - endpointPublicAccess=true + allow-list -> that allow-list, verbatim (explicit wins).
//   - endpointPublicAccess=true + empty list -> the cluster's LIVE allow-list when it already
//     exists, and on a first apply this host's egress IP as <ip>/32. Never 0.0.0.0/0 —
//     landing-zone rejects an empty allow-list at plan time, and so does this.
//
// The live list wins on a re-apply because an auto-detected value is a fact about whichever
// host ran the apply. Re-detecting it from another host, a laptop on a different network or
// a CI runner, re-scoped the endpoint to that host and silently locked out the operator who
// built it. Changing the list takes an explicit cluster.endpointAllowlist.
//
// A destroy never detects: it passes the live list when it can read it, and otherwise a
// placeholder that admits nobody, because the destroy plan needs the precondition to hold
// and nothing it does changes who reaches the API.
//
// Detection is dry-run-aware: a dry-run makes no network call and instead reports the value
// it would fetch and inject, so a plan never reaches out and never opens anything. The live
// list is read on a dry-run too, through Query, since reading it opens nothing.
func clusterEndpointEnv(ctx context.Context, st *engine.State, verb string) ([]string, error) {
	c := st.Config.Cluster
	env := []string{fmt.Sprintf("TF_VAR_cluster_endpoint_public_access=%t", c.EndpointPublicAccess)}

	if !c.EndpointPublicAccess {
		note(st, "EKS API endpoint: PRIVATE (public access off) — reach it from inside the VPC (bastion/VPN)")
		return env, nil
	}

	cidrs := c.EndpointAllowlist
	if len(cidrs) > 0 {
		note(st, "EKS API endpoint: PUBLIC, allow-listed to %s (explicit cluster.endpointAllowlist)", strings.Join(cidrs, ", "))
		return withCIDRs(env, cidrs)
	}

	live, absent, liveErr := liveAllowlist(ctx, st)
	switch {
	case verb == "destroy" && len(live) > 0:
		note(st, "EKS API endpoint: (destroy) passing the live allow-list %s so the destroy plan's "+
			"precondition holds — a destroy re-scopes nothing", strings.Join(live, ", "))
		return withCIDRs(env, live)
	case verb == "destroy":
		why := "the cluster no longer exists"
		if liveErr != nil {
			why = "the live allow-list could not be read (" + liveErr.Error() + ")"
		}
		note(st, "EKS API endpoint: (destroy) %s — passing %s, which admits nobody, only so the destroy "+
			"plan's precondition holds. No egress IP is detected on a destroy", why, destroyPlaceholderCIDR)
		return withCIDRs(env, []string{destroyPlaceholderCIDR})
	case len(live) > 0:
		note(st, "EKS API endpoint: PUBLIC, keeping the live allow-list %s. An empty "+
			"cluster.endpointAllowlist never re-scopes a cluster that exists; set it to change who reaches "+
			"the API", strings.Join(live, ", "))
		if !st.Runner.DryRun {
			note(st, "EKS API endpoint: %s", egressInside(ctx, live))
		}
		return withCIDRs(env, live)
	case liveErr != nil:
		return nil, fmt.Errorf("cluster.endpointPublicAccess is true with an empty cluster.endpointAllowlist, "+
			"and the live allow-list of %s could not be read: %w. rackctl does not auto-detect over a "+
			"cluster that may already be scoped to another host, which could lock its operator out. Set "+
			"cluster.endpointAllowlist explicitly to proceed", st.Config.ClusterName(), liveErr)
	case !absent:
		// The cluster exists with no allow-list at all, which the fail-closed tree never builds.
		return nil, fmt.Errorf("cluster.endpointPublicAccess is true with an empty cluster.endpointAllowlist, "+
			"and %s exists with no public allow-list to keep. Set cluster.endpointAllowlist explicitly to "+
			"proceed", st.Config.ClusterName())
	case st.Runner.DryRun:
		note(st, "EKS API endpoint: PUBLIC, empty allow-list — (apply) would detect this host's public egress "+
			"IP via %s and scope the endpoint to <that-ip>/32", ipEchoEndpoint)
		// Nothing to inject in dry-run: no network call is made, and a public endpoint with
		// no allow-list is exactly the combination landing-zone fails closed on — the CIDR is
		// supplied at --apply, once it is actually known.
		return env, nil
	}
	ip, err := egressResolver(ctx)
	if err != nil {
		return nil, fmt.Errorf("cluster.endpointPublicAccess is true with an empty cluster.endpointAllowlist, "+
			"so rackctl must detect this host's public egress IP to scope the API endpoint — detection failed: %w. "+
			"Set cluster.endpointAllowlist explicitly to proceed", err)
	}
	cidrs = []string{ip + "/32"}
	note(st, "EKS API endpoint: PUBLIC, allow-listed to %s (auto-detected operator egress IP); a re-apply "+
		"keeps it", cidrs[0])
	return withCIDRs(env, cidrs)
}

// withCIDRs appends the allow-list. Terragrunt/tofu read a list(string) TF_VAR as JSON.
func withCIDRs(env, cidrs []string) ([]string, error) {
	j, err := json.Marshal(cidrs)
	if err != nil {
		return nil, err
	}
	return append(env, "TF_VAR_cluster_endpoint_public_access_cidrs="+string(j)), nil
}

// liveAllowlist reads the public allow-list of this config's cluster as EKS holds it.
// absent reports that EKS says the cluster does not exist; any other failure is err.
func liveAllowlist(ctx context.Context, st *engine.State) (cidrs []string, absent bool, err error) {
	out, err := st.Runner.Query(ctx, "aws", "eks", "describe-cluster", "--name", st.Config.ClusterName(),
		"--region", st.Config.Cloud.Region,
		"--query", "cluster.resourcesVpcConfig.publicAccessCidrs", "--output", "json")
	if err != nil {
		if strings.Contains(err.Error(), "ResourceNotFoundException") {
			return nil, true, nil
		}
		return nil, false, err
	}
	if s := strings.TrimSpace(out); s == "" || s == "null" {
		return nil, false, nil
	}
	if err := json.Unmarshal([]byte(out), &cidrs); err != nil {
		return nil, false, fmt.Errorf("aws eks describe-cluster: the allow-list %q is not a list: %w", out, err)
	}
	return cidrs, false, nil
}

// egressInside says whether this host's egress IP falls inside the allow-list being kept.
// It informs and never decides: the list is kept either way.
func egressInside(ctx context.Context, cidrs []string) string {
	ip, err := egressResolver(ctx)
	if err != nil {
		return "this host's egress IP could not be detected (" + err.Error() + "), so whether it is inside " +
			"the allow-list is unknown"
	}
	addr := net.ParseIP(ip)
	for _, c := range cidrs {
		if _, n, err := net.ParseCIDR(c); err == nil && addr != nil && n.Contains(addr) {
			return "this host's egress IP " + ip + " is inside it"
		}
	}
	return "this host's egress IP " + ip + " is NOT in it, so the API will not answer from here; add it " +
		"through cluster.endpointAllowlist if this host should reach it"
}
