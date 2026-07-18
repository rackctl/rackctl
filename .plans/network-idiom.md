# Network idiom — rackctl

Tactical plan for this repo's target in the network-idiom campaign. Master plan:
`~/.claude/plans/network-idiom.md`.

**Nothing is deployed today** — no live hub cluster. Design for the cleanest shape;
no migration/backward-compat framing anywhere (greenfield doctrine).

## Status

| # | target | status |
|---|---|---|
| 7 | create-mode IPAM/TGW/centralized-egress knobs via the TF_VAR idiom | ✅ |

Ends in a PR (never a direct push to `main`), CI green (poll `gh pr checks`
synchronously in the foreground — no backgrounded `--watch`), then squash-merge.

---

## Target 7 — `create`-mode IPAM / TGW / centralized-egress knobs via the TF_VAR idiom (M)

**Depends on:** landing-zone Target 1 (the network var names this target injects)
AND **Target 3b** (`egress-network` — the owner-side receiving end for
`centralized_egress`) — both must be merged before starting. A second review pass
found `centralized_egress` originally had no owner-side implementation anywhere in
this campaign (flipping it would blackhole a cluster's egress); Target 3b builds
the missing piece (central-egress VPC + the TGW static default route). Confirm the
exact variable names landing-zone's Target 1 PR actually shipped by reading the
merged `components/aws/network/variables.tf` directly, not by assuming this plan's
proposed names are final — and confirm Target 3b actually merged before exposing
`CentralizedEgress` as a usable rackctl knob (day-0 bootstrap is the one place this
lever is most dangerous to leave inert, since day-0 by definition has no other
network infrastructure yet to fall back on).

**Findings:** rackctl drives the terragrunt live tree for day-0 hub bootstrap
(`live/aws/{account}/{region}/{env}/{component}/`) — it does **not** drive
`fleet/aws/cluster-stack` (that's the eks-fleet/Crossplane path for ongoing spoke
vends). `internal/config/config.go:141-144`'s `ClusterNet` struct has `VPCCIDR` +
`NATGateways` today. The established idiom (already shipped, Target 23 in the
quality-remediation campaign, `.plans/quality-remediation.md`, PR #34): `rackctl.yaml`
is the typed decision surface for durable choices; genuinely fragile/per-run knobs
get injected as `TF_VAR_*` environment variables onto the terragrunt runner at
`init --apply` time — precedent is `TF_VAR_cluster_name`
(`internal/phases/phases.go:372`) and the endpoint-posture vars (`:380-384`) — layered
over a committed live tree that stays generic and fail-closed. Day-0 bootstrap is
`create` mode by definition (the hub always mints its own VPC); the owner
`shared-network` account and any `adopt` spoke are operator-driven terragrunt outside
rackctl's day-0 scope (same as `org-networking` today, which is not a rackctl phase)
— day-0 `adopt` stays explicitly out of scope for this target.

**Approach:**
1. `ClusterNet`: add optional `IPAMPoolID`, `TransitGatewayID`, `CentralizedEgress`
   fields. Validate: `CentralizedEgress` requires `TransitGatewayID` set;
   `IPAMPoolID` and a non-default `VPCCIDR` are mutually exclusive — mirror landing-
   zone Target 1's own preconditions exactly, so a bad combination fails fast in
   rackctl's config validation rather than ~20 minutes into a `tofu apply`.
2. In the cluster phase's network-apply step (`internal/phases/phases.go`), append
   `TF_VAR_ipam_pool_id`, `TF_VAR_transit_gateway_id`, `TF_VAR_centralized_egress` to
   the terragrunt runner env when each is set — same placement and idiom as the
   existing `TF_VAR_cluster_name` injection.
3. `examples/rackctl.yaml` + README: document the three new fields as `create`-mode
   levers layered over the private-by-default committed live tree, and note plainly
   that day-0 bootstrap is `create` mode only — `adopt` is a spoke/eks-fleet concern,
   not a rackctl one.

**Acceptance:** `make test vet fmt` green; a dry-run
`rackctl init -c examples/rackctl.yaml` prints the TF_VARs it would inject without
applying anything; config validation rejects `centralizedEgress: true` without
`transitGatewayId` set, and rejects `ipamPoolId` set together with a non-default
`vpcCidr`.

**Shipped.** `ClusterNet` gained `IPAMPoolID` / `IPAMNetmaskLength` / `TransitGatewayID`
/ `CentralizedEgress`; each set lever rides a `TF_VAR_*` onto landing-zone's network
component in the cluster phase (`internal/phases/network.go`), same seam as
`TF_VAR_cluster_name`. `Validate` mirrors landing-zone's create-mode preconditions;
`examples/rackctl.yaml` + README document the levers as create-mode-only, layered over
the private-by-default committed tree. Discoveries:

- **A fourth knob — `ipamNetmaskLength` — was required, not the three the plan named.**
  landing-zone's `ipam_netmask_length` precondition (tightened to 16–20 by Target 1-fix)
  fails the plan whenever `ipam_pool_id` is set with the default netmask `0`, and the
  committed live network leaf sets neither — so injecting `ipam_pool_id` alone would die
  at plan. Because `transit_gateway_id` requires `ipam_pool_id` and `centralized_egress`
  requires `transit_gateway_id`, the *entire* lever chain is gated behind a working IPAM
  allocation, so all three plan-named knobs are unusable without the netmask. Added it as
  a real field + `TF_VAR_ipam_netmask_length`, validated 16–20 when a pool is set / 0
  otherwise, exactly mirroring landing-zone.
- **landing-zone has four create-mode preconditions on these knobs, not the two the plan
  enumerated.** Mirrored all four so a bad combination fails in `rackctl init` in a
  second: IPAM-pool ⇄ non-default-vpcCidr exclusion, IPAM netmask 16–20, transit-gateway
  requires an IPAM CIDR, centralized-egress requires a transit gateway.
- **`ApplyDefaults` bug fixed in passing.** It replaced the whole `ClusterNet` when
  `vpcCidr` was empty — which would silently wipe any lever set without a `vpcCidr` (the
  natural IPAM config, where the CIDR comes from the pool). Now defaults the two base
  fields individually. Regression test added.
