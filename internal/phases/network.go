package phases

import (
	"strconv"

	"github.com/rackctl/rackctl/internal/engine"
)

// clusterNetworkEnv builds the TF_VAR_* entries for the create-mode network levers —
// IPAM allocation, transit-gateway attachment, centralized egress — that rackctl layers
// onto landing-zone's network component in the cluster phase. Same seam as
// TF_VAR_cluster_name and the endpoint posture: landing-zone's committed tree is generic
// (a literal 10.0.0.0/16 VPC with local NAT), and rackctl supplies the fragile per-run
// choice that opts a day-0 hub into the org's IPAM/transit-gateway topology.
//
// Each lever is injected only when set, so an unset one leaves the committed default
// untouched. IPAMPoolID and IPAMNetmaskLength travel together (an IPAM allocation needs
// both). Config validation has already rejected any contradictory combination, so this
// only translates a valid config into env — it makes no decisions and no network calls.
//
// It prints each injected value as an operator-facing note, so a dry-run shows exactly
// what would reach the network module without applying anything.
func clusterNetworkEnv(st *engine.State) []string {
	n := st.Config.Cluster.Network
	var env []string

	if n.IPAMPoolID != "" {
		env = append(env,
			"TF_VAR_ipam_pool_id="+n.IPAMPoolID,
			"TF_VAR_ipam_netmask_length="+strconv.Itoa(n.IPAMNetmaskLength))
		note(st, "network: TF_VAR_ipam_pool_id=%s TF_VAR_ipam_netmask_length=%d — VPC CIDR drawn from the IPAM pool, not the literal vpcCidr",
			n.IPAMPoolID, n.IPAMNetmaskLength)
	}
	if n.TransitGatewayID != "" {
		env = append(env, "TF_VAR_transit_gateway_id="+n.TransitGatewayID)
		note(st, "network: TF_VAR_transit_gateway_id=%s — VPC attached to the transit gateway (10.0.0.0/8 routed to the TGW)", n.TransitGatewayID)
	}
	if n.CentralizedEgress {
		env = append(env, "TF_VAR_centralized_egress=true")
		note(st, "network: TF_VAR_centralized_egress=true — private default route via the transit gateway, zero local NAT gateways")
	}
	return env
}
