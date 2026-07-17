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
	defer resp.Body.Close()
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

// clusterEndpointEnv builds the TF_VAR_* entries that carry the operator's chosen EKS
// API-endpoint posture into landing-zone's cluster component, whose committed tree is
// private-by-default and fail-closed. It returns the env to append to the terragrunt runner
// after printing operator-facing notes about exactly what lands on the control plane — the
// CIDR the operator sees here is the CIDR the API server accepts.
//
// Posture:
//   - endpointPublicAccess=false            -> private endpoint; no CIDR list injected.
//   - endpointPublicAccess=true + allow-list -> that allow-list, verbatim (explicit wins).
//   - endpointPublicAccess=true + empty list -> auto-detect the operator's public egress IP
//     and scope the endpoint to <ip>/32. Never 0.0.0.0/0 — landing-zone rejects an empty
//     allow-list at plan time, and so does this.
//
// Detection is dry-run-aware: a dry-run makes no network call and instead reports the value
// it would fetch and inject, so a plan never reaches out and never opens anything.
func clusterEndpointEnv(ctx context.Context, st *engine.State) ([]string, error) {
	c := st.Config.Cluster
	env := []string{fmt.Sprintf("TF_VAR_cluster_endpoint_public_access=%t", c.EndpointPublicAccess)}

	if !c.EndpointPublicAccess {
		note(st, "EKS API endpoint: PRIVATE (public access off) — reach it from inside the VPC (bastion/VPN)")
		return env, nil
	}

	cidrs := c.EndpointAllowlist
	switch {
	case len(cidrs) > 0:
		note(st, "EKS API endpoint: PUBLIC, allow-listed to %s (explicit cluster.endpointAllowlist)", strings.Join(cidrs, ", "))
	case st.Runner.DryRun:
		note(st, "EKS API endpoint: PUBLIC, empty allow-list — (apply) would detect this host's public egress "+
			"IP via %s and scope the endpoint to <that-ip>/32", ipEchoEndpoint)
		// Nothing to inject in dry-run: no network call is made, and a public endpoint with
		// no allow-list is exactly the combination landing-zone fails closed on — the CIDR is
		// supplied at --apply, once it is actually known.
		return env, nil
	default:
		ip, err := egressResolver(ctx)
		if err != nil {
			return nil, fmt.Errorf("cluster.endpointPublicAccess is true with an empty cluster.endpointAllowlist, "+
				"so rackctl must detect this host's public egress IP to scope the API endpoint — detection failed: %w. "+
				"Set cluster.endpointAllowlist explicitly to proceed", err)
		}
		cidrs = []string{ip + "/32"}
		note(st, "EKS API endpoint: PUBLIC, allow-listed to %s (auto-detected operator egress IP)", cidrs[0])
	}

	// Terragrunt/tofu read a list(string) TF_VAR as JSON, so marshal the allow-list.
	j, err := json.Marshal(cidrs)
	if err != nil {
		return nil, err
	}
	return append(env, "TF_VAR_cluster_endpoint_public_access_cidrs="+string(j)), nil
}
