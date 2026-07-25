package config

import (
	"strings"
	"testing"
)

// adoptCfg is a minimal VALID adopt config: mode, a VPC, and three private subnets. Every
// create-mode lever is left at its default, which is the shape landing-zone's adopt guards
// require — those defaults ARE the sentinels they compare against.
// The baseline carries PUBLIC subnets too, so the private-only case below is a real
// variation rather than a no-op. Without that, adoptCfg() already had a nil public list and
// TestAdopt_NoPublicSubnetsIsValid asserted nil == nil — bit-for-bit the same config as
// TestAdopt_MinimalConfigIsValid, leaving the happy path WITH public subnets untested.
func adoptCfg(public ...string) *Config {
	c := valid()
	c.Cluster.Network.Mode = ModeAdopt
	c.Cluster.Network.AdoptVPCID = "vpc-0abc123"
	c.Cluster.Network.AdoptPrivateSubnetIDs = []string{"subnet-a1", "subnet-b2", "subnet-c3"}
	if public == nil {
		public = []string{"subnet-x9", "subnet-y8", "subnet-z7"}
	}
	c.Cluster.Network.AdoptPublicSubnetIDs = public
	return c
}

func errText(t *testing.T, c *Config) string {
	t.Helper()
	if err := c.Validate(); err != nil {
		return err.Error()
	}
	return ""
}

// The baseline. A config that says "adopt this VPC, here are its private subnets" and nothing
// else must be accepted — the create-mode levers all sit at defaults that adopt permits, so
// requiring the operator to clear anything would be a bug in the guards.
func TestAdopt_MinimalConfigIsValid(t *testing.T) {
	if got := errText(t, adoptCfg()); got != "" {
		t.Fatalf("a minimal adopt config must validate; the create-mode levers are at their "+
			"defaults, which is exactly what landing-zone's adopt guards compare against.\ngot: %s", got)
	}
}

// A private-only adopt cluster is a supported shape — landing-zone blesses it explicitly
// ("Empty is valid for a private-only cluster") — so an empty public list must not be an error.
func TestAdopt_NoPublicSubnetsIsValid(t *testing.T) {
	c := adoptCfg([]string{}...) // explicitly no public subnets, unlike the baseline
	if got := errText(t, c); got != "" {
		t.Fatalf("a private-only adopt cluster is valid upstream; rackctl must not reject it.\ngot: %s", got)
	}
}

// The six create-mode levers landing-zone rejects under adopt, each checked here so the
// contradiction surfaces in a second instead of seconds-to-minutes into a tofu run naming a
// Terraform variable the operator never typed.
//
// Two of them — vpcCidr and natGateways — are compared against a DEFAULT rather than an empty
// value, because ApplyDefaults forces both and so neither has an unset state: the default is
// the sentinel. natGateways is the one where getting that wrong would be silent: ApplyDefaults
// forces it to 1, so a guard written against 0 would never fire.
func TestAdopt_RejectsEveryCreateModeLever(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"vpcCidr", func(c *Config) { c.Cluster.Network.VPCCIDR = "10.42.0.0/16" }, "cluster.network.vpcCidr"},
		{"natGateways", func(c *Config) { c.Cluster.Network.NATGateways = 3 }, "cluster.network.natGateways"},
		{"ipamPoolId", func(c *Config) { c.Cluster.Network.IPAMPoolID = "ipam-pool-0abc" }, "cluster.network.ipamPoolId"},
		{"transitGatewayId", func(c *Config) { c.Cluster.Network.TransitGatewayID = "tgw-0abc" }, "cluster.network.transitGatewayId"},
		{"centralizedEgress", func(c *Config) { c.Cluster.Network.CentralizedEgress = true }, "cluster.network.centralizedEgress"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := adoptCfg()
			tc.mutate(c)
			got := errText(t, c)
			if got == "" {
				t.Fatalf("%s is a create-mode lever with an adopt-rejecting validation upstream — "+
					"setting it under adopt must fail here, not at tofu validate", tc.name)
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("the error must name the config field the operator typed (%s); got: %s", tc.want, got)
			}
		})
	}
}

// ipamNetmaskLength deliberately has NO adopt guard, and that is not an oversight to be
// tidied up later. Its landing-zone validation references only ipam_pool_id, never
// network_mode, so a pool set under adopt fails on ipam_pool_id alone — one error, naming the
// real mistake. Adding a second guard here would report two errors for one mistake and imply
// that adjusting the netmask could help.
func TestAdopt_IPAMPoolUnderAdoptReportsOneErrorNotTwo(t *testing.T) {
	// Both netmask values, because they exercise different arms. 16 is valid FOR a pool, so
	// the create-mode rule would pass anyway; 0 is what that rule REJECTS when a pool is set,
	// which is the case that proves Validate's adopt arm is being taken at all. Testing only
	// 16 left the arm unpinned — deleting it entirely kept the whole suite green.
	for _, netmask := range []int{16, 0} {
		c := adoptCfg()
		c.Cluster.Network.IPAMPoolID = "ipam-pool-0abc"
		c.Cluster.Network.IPAMNetmaskLength = netmask

		got := errText(t, c)
		if !strings.Contains(got, "cluster.network.ipamPoolId") {
			t.Fatalf("netmask=%d: the pool itself must be rejected; got: %s", netmask, got)
		}
		if strings.Contains(got, "ipamNetmaskLength") {
			t.Fatalf("netmask=%d: ipamNetmaskLength must not be reported — with a pool set it is "+
				"beside the point, and naming it here suggests changing the netmask would help.\ngot: %s",
				netmask, got)
		}
	}
}

// The hole the adopt arm's condition closes. Skipping the create-mode relationship checks for
// `adopt` outright — rather than for `adopt AND a pool` — made adopt strictly MORE permissive
// than create for one field: this config validated clean while the identical create config was
// rejected. And adoptEnv never injects ipam_netmask_length, so the value was dropped
// invisibly, absent even from a dry-run.
//
// The reachable path: an operator converting a create config to adopt comments out ipamPoolId
// and forgets the netmask on the line beneath it. Nothing said anything, and the stray line
// stayed in their committed rackctl.yaml looking load-bearing.
func TestAdopt_RejectsANetmaskWithNoPool(t *testing.T) {
	c := adoptCfg()
	c.Cluster.Network.IPAMNetmaskLength = 18 // no pool set

	if got := errText(t, c); !strings.Contains(got, "ipamNetmaskLength") {
		t.Fatalf("a netmask with no ipamPoolId must be rejected under adopt exactly as it is under "+
			"create — it reaches nothing either way, and adopt must not be the laxer mode.\ngot: %s", got)
	}
}

// The mirror image: adopt inputs under create mode. landing-zone rejects these too — create
// builds its own VPC and subnets, so a reference to a foreign one is a contradiction rather
// than something to ignore.
func TestCreate_RejectsAdoptInputs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"adoptVpcId", func(c *Config) { c.Cluster.Network.AdoptVPCID = "vpc-0abc" }},
		{"adoptPrivateSubnetIds", func(c *Config) { c.Cluster.Network.AdoptPrivateSubnetIDs = []string{"subnet-a"} }},
		{"adoptPublicSubnetIds", func(c *Config) { c.Cluster.Network.AdoptPublicSubnetIDs = []string{"subnet-a"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := valid() // create by omission
			tc.mutate(c)
			if got := errText(t, c); !strings.Contains(got, tc.name) {
				t.Fatalf("%s under create mode must be rejected, matching landing-zone; got: %s", tc.name, got)
			}
		})
	}
}

func TestAdopt_RequiresVPCAndPrivateSubnets(t *testing.T) {
	c := adoptCfg()
	c.Cluster.Network.AdoptVPCID = ""
	c.Cluster.Network.AdoptPrivateSubnetIDs = nil

	got := errText(t, c)
	for _, want := range []string{"adoptVpcId is required", "adoptPrivateSubnetIds is required"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in: %s", want, got)
		}
	}
}

// A malformed id is caught here because landing-zone cannot: it finds out at DescribeVpcs /
// DescribeSubnets, as a raw provider error against a live AWS call, after the plan has
// started.
func TestAdopt_CatchesMalformedIDs(t *testing.T) {
	c := adoptCfg()
	c.Cluster.Network.AdoptVPCID = "vpc0abc123" // missing the hyphen
	c.Cluster.Network.AdoptPrivateSubnetIDs = []string{"subnet-a1", "sunbet-b2", "subnet-c3"}

	got := errText(t, c)
	if !strings.Contains(got, "does not look like a VPC id") {
		t.Errorf("a VPC id without the vpc- prefix must be caught; got: %s", got)
	}
	if !strings.Contains(got, "adoptPrivateSubnetIds[1]") {
		t.Errorf("the error must say WHICH entry is malformed; got: %s", got)
	}
}

// landing-zone accepts a subnet listed in both tiers silently — adopt.tf builds the two
// for_each sets independently and re-exports both raw lists — so the id lands in the private
// AND public tier of every downstream consumer. Nothing upstream can catch it: a variable
// validation cannot reach across two variables' element sets.
func TestAdopt_RejectsASubnetInBothTiers(t *testing.T) {
	c := adoptCfg()
	c.Cluster.Network.AdoptPublicSubnetIDs = []string{"subnet-b2", "subnet-x9", "subnet-y8"}

	got := errText(t, c)
	if !strings.Contains(got, "subnet-b2") || !strings.Contains(got, "both") {
		t.Fatalf("a subnet in both tiers must be rejected and named — landing-zone accepts it "+
			"silently and it lands in both tiers of every consumer.\ngot: %s", got)
	}
}

// The AZ-coverage necessary condition. landing-zone asserts the adopted private subnets span
// at least max_azs (3) distinct zones, at plan time, via a real DescribeSubnets. rackctl
// cannot know a subnet's zone — but N distinct subnets can never cover more than N zones.
func TestAdopt_RejectsTooFewPrivateSubnetsToSpanTheZones(t *testing.T) {
	c := adoptCfg()
	c.Cluster.Network.AdoptPrivateSubnetIDs = []string{"subnet-a1", "subnet-b2"}

	if got := errText(t, c); !strings.Contains(got, "at least 3") {
		t.Fatalf("two subnets cannot span the three availability zones landing-zone asserts, so "+
			"this fails at plan against live AWS — catch it from the config alone.\ngot: %s", got)
	}
}

// And the bound must count DISTINCT ids, not length. landing-zone keys its subnet data source
// on toset(ids) and counts distinct zones over those instances, so three copies of one subnet
// is one zone. A length check would pass a config that cannot possibly plan.
func TestAdopt_DuplicateSubnetsDoNotSatisfyTheZoneSpread(t *testing.T) {
	c := adoptCfg()
	c.Cluster.Network.AdoptPrivateSubnetIDs = []string{"subnet-a1", "subnet-a1", "subnet-a1"}

	if got := errText(t, c); !strings.Contains(got, "1 distinct") {
		t.Fatalf("three copies of one subnet is one availability zone, and landing-zone counts "+
			"distinct zones over toset(ids) — a length check would let this through.\ngot: %s", got)
	}
}

func TestNetworkMode_RejectsAnythingElse(t *testing.T) {
	c := valid()
	c.Cluster.Network.Mode = "shared"
	if got := errText(t, c); !strings.Contains(got, "cluster.network.mode must be") {
		t.Fatalf("an unknown mode must be rejected by name; got: %s", got)
	}
}

// The empty mode is create, matching landing-zone's network_mode default and the committed
// leaves, which select create by omission. Adopt() is what every branch keys on, so it has to
// agree.
func TestNetworkMode_EmptyMeansCreate(t *testing.T) {
	if (ClusterNet{}).Adopt() {
		t.Fatal("an omitted mode must mean create — landing-zone defaults network_mode to create " +
			"and every committed workload leaf selects it by omission")
	}
	if !(ClusterNet{Mode: ModeAdopt}).Adopt() {
		t.Fatal("ModeAdopt must report Adopt()")
	}
}

// The create-mode levers must still be validated against each other, exactly as before —
// adding a mode must not have made the existing IPAM/TGW rules conditional on anything.
func TestCreate_LeversStillValidateAgainstEachOther(t *testing.T) {
	c := valid()
	c.Cluster.Network.TransitGatewayID = "tgw-0abc" // requires an IPAM pool
	if got := errText(t, c); !strings.Contains(got, "requires an IPAM-allocated CIDR") {
		t.Fatalf("the create-mode relationships must be unchanged; got: %s", got)
	}
}
