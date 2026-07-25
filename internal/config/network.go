package config

import (
	"fmt"
	"strings"
)

// Adopt reports whether this platform participates in a VPC it does not own.
//
// The empty mode is create, matching landing-zone's own `network_mode` default and the
// committed workload leaves, which select create by omission.
func (n ClusterNet) Adopt() bool { return n.Mode == ModeAdopt }

// validateNetworkMode enforces the create ⇄ adopt contract.
//
// Every rule here mirrors a `validation` block on landing-zone's network component, plus four
// that landing-zone cannot express in a variable block at all. Mirroring is the point: tofu
// would reject the same combinations, but seconds-to-minutes into a run, with a message
// naming a Terraform variable rather than the config field the operator actually typed.
//
// The exception list is as important as the rules, because "mirror it consistently" is wrong
// here in two places, and landing-zone's own comments say so:
//
//   - max_azs is a BOTH-modes input. adopt.tf uses it to assert the adopted private subnets
//     span at least that many zones, and there is an upstream regression test
//     (adopt_accepts_max_azs) pinning it accepted with a non-default value. rackctl does not
//     expose it; if it ever does, it must not carry an adopt guard.
//   - enable_s3_gateway_endpoint, enable_interface_endpoints and enable_eks_interface_endpoint
//     are create-only but default TRUE, so an adopt-rejecting guard would reject every adopt
//     leaf's own defaults. They are inert under adopt (the owner runs endpoints), and there is
//     no unset state to tell "left alone" from "deliberately on". rackctl exposes none of them.
func validateNetworkMode(n ClusterNet) []string {
	var errs []string

	switch n.Mode {
	case "", ModeCreate, ModeAdopt:
	default:
		return []string{fmt.Sprintf("cluster.network.mode must be %q or %q (or omitted, which means %q), got %q",
			ModeCreate, ModeAdopt, ModeCreate, n.Mode)}
	}

	if !n.Adopt() {
		// create builds its own VPC and subnets, so a reference to a foreign one is a
		// contradiction rather than a no-op — landing-zone rejects each of these too.
		if n.AdoptVPCID != "" {
			errs = append(errs, fmt.Sprintf("cluster.network.adoptVpcId (%q) applies only with cluster.network.mode: adopt — create mode builds its own VPC", n.AdoptVPCID))
		}
		if len(n.AdoptPrivateSubnetIDs) > 0 {
			errs = append(errs, "cluster.network.adoptPrivateSubnetIds applies only with cluster.network.mode: adopt — create mode builds its own subnets")
		}
		if len(n.AdoptPublicSubnetIDs) > 0 {
			errs = append(errs, "cluster.network.adoptPublicSubnetIds applies only with cluster.network.mode: adopt — create mode builds its own subnets")
		}
		return errs
	}

	// ---- adopt: what must be present -------------------------------------------------

	if n.AdoptVPCID == "" {
		errs = append(errs, "cluster.network.adoptVpcId is required with cluster.network.mode: adopt — there is no VPC to participate in without it")
	}
	if len(n.AdoptPrivateSubnetIDs) == 0 {
		errs = append(errs, "cluster.network.adoptPrivateSubnetIds is required and must be non-empty with cluster.network.mode: adopt — the system node group and every pod run in the private subnets")
	}

	// ---- adopt: the six create-mode levers it rejects ---------------------------------
	//
	// TWO of these — vpcCidr and natGateways — are compared against their DEFAULT rather than
	// against an empty value, because ApplyDefaults forces both and they therefore have no
	// unset state: the default IS the sentinel. That is the same shape landing-zone uses
	// (`var.vpc_cidr == "10.0.0.0/16"`, `var.nat_gateways == 1`), and comparing natGateways
	// against 0 instead would be wrong — ApplyDefaults has already forced it to 1. The other
	// three have no meaningful default, so empty/false is the sentinel.

	if n.VPCCIDR != "" && n.VPCCIDR != defaultVPCCIDR {
		errs = append(errs, fmt.Sprintf("cluster.network.vpcCidr (%q) is a create-mode lever and does not apply with cluster.network.mode: adopt — an adopted VPC's CIDR is the owner's, and landing-zone reads it back from the VPC (data.aws_vpc.adopt). Leave it unset", n.VPCCIDR))
	}
	if n.NATGateways != 0 && n.NATGateways != 1 {
		errs = append(errs, fmt.Sprintf("cluster.network.natGateways (%d) is a create-mode lever and does not apply with cluster.network.mode: adopt — the VPC owner runs egress for a shared VPC. Leave it unset", n.NATGateways))
	}
	if n.IPAMPoolID != "" {
		errs = append(errs, fmt.Sprintf("cluster.network.ipamPoolId (%q) is a create-mode lever and does not apply with cluster.network.mode: adopt — an adopted VPC's CIDR was already allocated by its owner", n.IPAMPoolID))
	}
	if n.TransitGatewayID != "" {
		errs = append(errs, fmt.Sprintf("cluster.network.transitGatewayId (%q) is a create-mode lever and does not apply with cluster.network.mode: adopt — the VPC owner runs the transit-gateway attachment", n.TransitGatewayID))
	}
	if n.CentralizedEgress {
		errs = append(errs, "cluster.network.centralizedEgress is a create-mode lever and does not apply with cluster.network.mode: adopt — the VPC owner runs egress for a shared VPC")
	}
	// ipamNetmaskLength gets no adopt guard of its own, deliberately. Its landing-zone
	// validation references only ipam_pool_id, never network_mode, so an adopt guard here
	// would report two errors for one mistake and imply that adjusting the netmask could
	// help. It is constrained from two directions instead: WITH a pool, the pool is rejected
	// just above and the netmask is beside the point; WITHOUT one, Validate's
	// `IPAMPoolID == ""` case rejects a non-zero netmask in BOTH modes — which it only does
	// because that switch skips its relationship checks for `adopt AND a pool` rather than
	// for adopt outright. Skipping on adopt alone left this field unconstrained under adopt
	// and constrained under create, and adoptEnv never injects it, so the value was dropped
	// with nothing said. See the switch in Validate.

	// ---- adopt: the four checks landing-zone cannot make -----------------------------
	//
	// These are rackctl's alone. A variable block cannot reach across two variables' element
	// sets, and it cannot cheaply reject a malformed id — landing-zone finds those at
	// DescribeVpcs / DescribeSubnets, as a raw provider error against a live AWS call.

	if n.AdoptVPCID != "" && !strings.HasPrefix(n.AdoptVPCID, "vpc-") {
		errs = append(errs, fmt.Sprintf("cluster.network.adoptVpcId (%q) does not look like a VPC id — expected vpc-<hex>. Left to AWS this fails at DescribeVpcs with a raw provider error", n.AdoptVPCID))
	}
	for i, id := range n.AdoptPrivateSubnetIDs {
		if !strings.HasPrefix(id, "subnet-") {
			errs = append(errs, fmt.Sprintf("cluster.network.adoptPrivateSubnetIds[%d] (%q) does not look like a subnet id — expected subnet-<hex>", i, id))
		}
	}
	for i, id := range n.AdoptPublicSubnetIDs {
		if !strings.HasPrefix(id, "subnet-") {
			errs = append(errs, fmt.Sprintf("cluster.network.adoptPublicSubnetIds[%d] (%q) does not look like a subnet id — expected subnet-<hex>", i, id))
		}
	}

	// A subnet in both tiers is silently accepted by landing-zone — adopt.tf builds the two
	// for_each sets independently and re-exports both raw lists — so the id lands in the
	// private AND the public tier of every downstream consumer. A subnet is one or the other.
	public := make(map[string]bool, len(n.AdoptPublicSubnetIDs))
	for _, id := range n.AdoptPublicSubnetIDs {
		public[id] = true
	}
	for _, id := range n.AdoptPrivateSubnetIDs {
		if public[id] {
			errs = append(errs, fmt.Sprintf("subnet %s is listed in both cluster.network.adoptPrivateSubnetIds and adoptPublicSubnetIds — a subnet is private or public, not both. landing-zone accepts this silently and the subnet lands in both tiers of every consumer", id))
		}
	}

	// The AZ-coverage necessary condition. landing-zone asserts the adopted private subnets
	// span at least max_azs (3) distinct availability zones, at plan time, via a real
	// DescribeSubnets. rackctl cannot know a subnet's zone without that call — but N distinct
	// subnets can never cover more than N zones, so too few ids is a guaranteed failure that
	// is free to catch here.
	//
	// Counting DISTINCT ids rather than length is what makes the bound sound: landing-zone
	// keys its data source on toset(ids) and counts distinct zones over those instances, so
	// three copies of one subnet is one zone, and a length check would pass a config that
	// cannot.
	if len(n.AdoptPrivateSubnetIDs) > 0 {
		seen := make(map[string]bool, len(n.AdoptPrivateSubnetIDs))
		for _, id := range n.AdoptPrivateSubnetIDs {
			seen[id] = true
		}
		if len(seen) < adoptMinPrivateSubnets {
			errs = append(errs, fmt.Sprintf("cluster.network.adoptPrivateSubnetIds has %d distinct subnet(s); adopt needs at least %d, one per availability zone. "+
				"landing-zone asserts the adopted private subnets span at least max_azs (%d) zones so the system node group can spread across them, "+
				"and %d subnets cannot cover %d zones",
				len(seen), adoptMinPrivateSubnets, adoptMinPrivateSubnets, len(seen), adoptMinPrivateSubnets))
		}
	}

	return errs
}
