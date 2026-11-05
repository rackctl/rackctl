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
//     exists with a public endpoint, and on a first apply, or over a cluster whose endpoint is
//     private, this host's egress IP as <ip>/32. Never 0.0.0.0/0 — landing-zone rejects an
//     empty allow-list at plan time, and so does this.
//
// The live list wins on a re-apply because an auto-detected value is a fact about whichever
// host ran the apply. Re-detecting it from another host, a laptop on a different network or
// a CI runner, re-scoped the endpoint to that host and silently locked out the operator who
// built it. Changing the list takes an explicit cluster.endpointAllowlist.
//
// The live list counts only while the live endpoint is public. EKS reports 0.0.0.0/0 as the
// allow-list of a cluster whose public endpoint is off, so keeping that value when public
// access goes from off to on would open the API to the whole internet. A private cluster
// admits nobody through its public allow-list, so detection there locks nobody out. A live
// public list that admits every address is never kept implicitly either: opening the
// endpoint to the world takes an explicit cluster.endpointAllowlist.
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
	public := st.Config.PublicEndpoint()
	env := []string{fmt.Sprintf("TF_VAR_cluster_endpoint_public_access=%t", public)}
	note(st, "EKS API endpoint: public access %t, %s", public, endpointSource(st))

	if !public {
		note(st, "EKS API endpoint: PRIVATE (public access off) — reach it from inside the VPC (bastion/VPN)")
		return env, nil
	}

	cidrs := c.EndpointAllowlist
	if len(cidrs) > 0 {
		note(st, "EKS API endpoint: PUBLIC, allow-listed to %s (explicit cluster.endpointAllowlist)", strings.Join(cidrs, ", "))
		return withCIDRs(env, cidrs)
	}

	live, absent, private, liveErr := liveAllowlist(ctx, st)
	if private && verb != "destroy" {
		note(st, "EKS API endpoint: %s exists with its public endpoint off, so it has no public allow-list to "+
			"keep; this apply scopes the newly public endpoint as a first apply would", st.Config.ClusterName())
	}
	switch {
	case verb != "destroy" && worldOpen(live) != "":
		return nil, fmt.Errorf("cluster.endpointPublicAccess is true with an empty cluster.endpointAllowlist, "+
			"and the live allow-list of %s admits every address (%s). rackctl keeps a live allow-list on a "+
			"re-apply but never keeps one open to the whole internet without being told to. Set "+
			"cluster.endpointAllowlist explicitly to proceed", st.Config.ClusterName(), worldOpen(live))
	case verb == "destroy" && len(live) > 0:
		note(st, "EKS API endpoint: (destroy) passing the live allow-list %s so the destroy plan's "+
			"precondition holds — a destroy re-scopes nothing", strings.Join(live, ", "))
		return withCIDRs(env, live)
	case verb == "destroy":
		why := "the cluster no longer exists"
		if !absent && liveErr == nil {
			why = "the cluster's public endpoint is off, so it has no public allow-list"
		}
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
	case !absent && !private:
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

// endpointSource says where the public-access value came from: the file, or the
// environment's default, which is public in development and private elsewhere.
func endpointSource(st *engine.State) string {
	if st.Config.IsDefaulted("cluster.endpointPublicAccess") {
		return fmt.Sprintf("the %s default (cluster.endpointPublicAccess is not set; set it to choose)",
			st.Config.Environment)
	}
	return "from cluster.endpointPublicAccess"
}

// withCIDRs appends the allow-list. Terragrunt/tofu read a list(string) TF_VAR as JSON.
func withCIDRs(env, cidrs []string) ([]string, error) {
	j, err := json.Marshal(cidrs)
	if err != nil {
		return nil, err
	}
	return append(env, "TF_VAR_cluster_endpoint_public_access_cidrs="+string(j)), nil
}

// liveAllowlist reads the public allow-list of this config's cluster as EKS holds it, and
// whether that cluster's public endpoint is on. A cluster whose public endpoint is off has
// no public allow-list: EKS still reports one, 0.0.0.0/0 by default, and that value admits
// nobody until public access is turned on, so it is returned as no list with private set.
// absent reports that EKS says the cluster does not exist; any other failure is err.
func liveAllowlist(ctx context.Context, st *engine.State) (cidrs []string, absent, private bool, err error) {
	out, err := st.Runner.Query(ctx, "aws", "eks", "describe-cluster", "--name", st.Config.ClusterName(),
		"--region", st.Config.Cloud.Region,
		"--query", "cluster.resourcesVpcConfig.{public:endpointPublicAccess,cidrs:publicAccessCidrs}",
		"--output", "json")
	if err != nil {
		if strings.Contains(err.Error(), "ResourceNotFoundException") {
			return nil, true, false, nil
		}
		return nil, false, false, err
	}
	var live struct {
		Public *bool    `json:"public"`
		CIDRs  []string `json:"cidrs"`
	}
	if err := json.Unmarshal([]byte(out), &live); err != nil {
		return nil, false, false, fmt.Errorf("aws eks describe-cluster: the endpoint posture %q is not an "+
			"object: %w", strings.TrimSpace(out), err)
	}
	if live.Public == nil {
		return nil, false, false, fmt.Errorf("aws eks describe-cluster: the endpoint posture %q does not say "+
			"whether the public endpoint is on", strings.TrimSpace(out))
	}
	if !*live.Public {
		return nil, false, true, nil
	}
	return live.CIDRs, false, false, nil
}

// worldOpen returns the first entry of cidrs that admits every address, or "" when none
// does. Any prefix of length zero qualifies, however its address is spelled.
func worldOpen(cidrs []string) string {
	for _, c := range cidrs {
		if _, n, err := net.ParseCIDR(strings.TrimSpace(c)); err == nil {
			if ones, _ := n.Mask.Size(); ones == 0 {
				return c
			}
		}
	}
	return ""
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
