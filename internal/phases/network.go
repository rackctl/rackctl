package phases

import (
	"encoding/json"
	"strconv"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/engine"
)

// clusterNetworkEnv builds the TF_VAR_* entries for landing-zone's network component: the
// mode, then either the create-mode levers (IPAM allocation, transit-gateway attachment,
// centralized egress) or the adopt inputs. Same seam as TF_VAR_cluster_name and the endpoint
// posture — landing-zone's committed tree is generic (a literal 10.0.0.0/16 VPC with local
// NAT), and rackctl supplies the fragile per-run choice layered over it.
//
// Each create-mode lever is injected only when set, so an unset one leaves the committed
// default untouched. IPAMPoolID and IPAMNetmaskLength travel together (an IPAM allocation
// needs both). Config validation has already rejected any contradictory combination, so this
// only translates a valid config into env — it makes no decisions and no network calls.
//
// It prints each injected value as an operator-facing note, so a dry-run shows exactly
// what would reach the network module without applying anything.
func clusterNetworkEnv(st *engine.State, verb string) []string {
	n := st.Config.Cluster.Network
	var env []string

	// network_mode goes out on every run, in both modes, and that is a deliberate exception
	// to the "inject only what differs from the default" rule the other levers follow.
	//
	// The rule exists because an ambient TF_VAR beats a leaf's `inputs`, so injecting a
	// default-valued knob silently overrides whatever that environment's leaf carefully
	// chose. That reasoning applies to per-environment TUNING — natGateways is the clean
	// example: staging and production pin 3 for per-AZ HA, and rackctl's own default is 1, so
	// wiring it unconditionally would quietly downgrade both to a single shared NAT gateway.
	//
	// Mode is not tuning. It is the operator's declaration about who owns the VPC, and the
	// two answers are not interchangeable. Every committed workload leaf selects create by
	// omission, so sending "create" changes nothing today — and if a leaf ever pins adopt, an
	// operator whose rackctl.yaml says create must get create rather than a silent adopt.
	env = append(env, "TF_VAR_network_mode="+string(mode(n)))

	if n.Adopt() {
		return append(env, adoptEnv(st, n, verb)...)
	}

	// Noted even though it asserts the committed default, because it is injected either way
	// and an override nobody can see in a dry-run is the class of problem this campaign keeps
	// finding. One line; it says what mode this run lands and what that means for the VPC.
	note(st, "network: TF_VAR_network_mode=create — this platform owns its VPC (%s here, with its "+
		"subnets, endpoints and egress). Set cluster.network.mode: adopt to join a VPC another "+
		"account owns instead", map[bool]string{true: "destroyed", false: "built"}[verb == "destroy"])

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

	// Sizing knobs: only when they differ from Default(). Staging and production pin
	// nat_gateways=3 for per-AZ HA; rackctl's default is 1. Injecting the default would
	// quietly downgrade both environments to a single shared NAT — the exact class of
	// silent override this campaign exists to kill. A non-default value is the operator's
	// deliberate choice and must reach the leaf.
	d := config.Default().Cluster.Network
	if n.VPCCIDR != "" && n.VPCCIDR != d.VPCCIDR {
		env = append(env, "TF_VAR_vpc_cidr="+n.VPCCIDR)
		note(st, "network: TF_VAR_vpc_cidr=%s — literal CIDR override (mutually exclusive with IPAM)", n.VPCCIDR)
	}
	if n.NATGateways != 0 && n.NATGateways != d.NATGateways {
		env = append(env, "TF_VAR_nat_gateways="+strconv.Itoa(n.NATGateways))
		note(st, "network: TF_VAR_nat_gateways=%d — overrides the leaf (staging/production pin 3; "+
			"development inherits the component default of 1)", n.NATGateways)
	}
	return env
}

// mode resolves the network mode, treating the empty value as create — matching
// landing-zone's own `network_mode` default and the committed leaves, which select create by
// omission rather than by stating it.
func mode(n config.ClusterNet) config.NetworkMode {
	if n.Mode == "" {
		return config.ModeCreate
	}
	return n.Mode
}

// adoptEnv builds the adopt-mode TF_VARs: the three inputs the component needs, plus two
// neutralizers without which adopt cannot be selected in staging or production at all.
//
// The lists are JSON because adopt_private_subnet_ids and adopt_public_subnet_ids are
// list(string), and TF_VAR_ carries a complex type as an HCL expression, which JSON array
// syntax satisfies.
//
// Config validation has already rejected every contradictory combination, so this makes no
// decisions and no AWS calls — it translates a valid config into environment.
func adoptEnv(st *engine.State, n config.ClusterNet, verb string) []string {
	priv, _ := json.Marshal(n.AdoptPrivateSubnetIDs)
	pub := []byte("[]")
	if len(n.AdoptPublicSubnetIDs) > 0 {
		pub, _ = json.Marshal(n.AdoptPublicSubnetIDs)
	}

	env := []string{
		"TF_VAR_adopt_vpc_id=" + n.AdoptVPCID,
		"TF_VAR_adopt_private_subnet_ids=" + string(priv),
		"TF_VAR_adopt_public_subnet_ids=" + string(pub),

		// The two neutralizers, and they are what make adopt reachable outside development.
		//
		// nat_gateways and enable_flow_logs both carry an adopt-rejecting validation upstream,
		// and the staging and production network leaves pin exactly the values that trip them:
		// nat_gateways = 3 and enable_flow_logs = true. Those are correct create-mode choices
		// for those environments — per-AZ NAT redundancy, and flow logs on — so the leaves are
		// not wrong; they simply describe a VPC this platform would be building rather than
		// joining. Without these two overrides, `mode: adopt` fails tofu validation on both
		// variables in staging and production while working in development, which is the least
		// useful possible behaviour.
		//
		// Overriding them is semantically empty rather than a real change: under adopt neither
		// input builds anything (their resources are gated on create mode) and the VPC owner
		// runs both egress and flow logging — an adopting participant cannot log a VPC it does
		// not own. So this sets them to the values that mean "not mine", which is the truth.
		"TF_VAR_nat_gateways=1",
		"TF_VAR_enable_flow_logs=false",
	}

	note(st, "network: TF_VAR_network_mode=adopt — participating in VPC %s, which this platform does "+
		"not own: no VPC, subnets, endpoints or egress are %s here. The owner runs those; "+
		"landing-zone re-exports the adopted values through the same outputs, so the cluster wires "+
		"identically to a created VPC", n.AdoptVPCID,
		map[bool]string{true: "destroyed", false: "built"}[verb == "destroy"])
	note(st, "network: %d private subnet(s) %s", len(n.AdoptPrivateSubnetIDs), string(priv))
	if len(n.AdoptPublicSubnetIDs) > 0 {
		note(st, "network: %d public subnet(s) %s", len(n.AdoptPublicSubnetIDs), string(pub))
	} else {
		note(st, "network: no public subnets — a private-only adopt cluster. Internal load balancers "+
			"work; an internet-facing Service or Ingress will NOT provision, because the Kyverno rule "+
			"that injects load-balancer subnets guards on a non-empty public list")
	}
	note(st, "network: TF_VAR_nat_gateways=1 TF_VAR_enable_flow_logs=false — neutralizing the two "+
		"create-mode levers this environment's leaf pins (staging and production set 3 and true), "+
		"which landing-zone rejects under adopt. Both are the owner's concern for a VPC we do not own")
	if verb != "destroy" {
		note(st, "network: landing-zone asserts the rest at plan time against live AWS — every subnet is in "+
			"VPC %s, each private route table routes to the S3 gateway prefix list and to a live NAT or "+
			"transit gateway, and the private subnets span at least 3 availability zones", n.AdoptVPCID)
	}

	return env
}
