# Quality remediation — rackctl

Tactical plan for this repo's target in the org quality-remediation campaign.
Master plan: `~/.claude/plans/quality-remediation.md` (decision U3 is settled: rackctl
owns the fragile endpoint input; landing-zone's committed tree goes private-by-default
in its target 24, which depends on this one shipping first).

## Status

| # | target | status |
|---|---|---|
| 23 | Endpoint knob wiring + preflight secret names | ✅ #34 |

---

## Target 23 — Endpoint knob wiring + preflight secret names (M)

Context — the idiom this repo already established, which this target completes:
- `rackctl.yaml` is the typed decision surface: `cluster.endpointPublicAccess` exists
  (`internal/config/config.go:110`), defaults `true` (`config.go:190`), and is validated
  false-for-production (`config.go:273-274`).
- Durable per-account facts become files once (phase 2 identity runs landing-zone's
  `scripts/init-backend-aws.sh` → `account.hcl` + state backend).
- Per-run fragile knobs ride `TF_VAR_*` on the terragrunt runner env at `init --apply` —
  the precedent is `TF_VAR_cluster_name` (`internal/phases/phases.go:372`), layered over
  a committed live tree that stays generic and fail-closed.

Findings:
- `EndpointPublicAccess` is schema + validation only — consumed nowhere in
  `internal/phases/`. The knob is declared; landing-zone's committed flags are doing the
  work (and failing their own guard).
- No allowlist field exists in the config schema; no egress-IP detection exists anywhere.
- `internal/preflight/preflight.go:318` expects secrets named `eks-grafana-token` and
  `eks-managed-monitoring-endpoints` — pre-rename residue; landing-zone now creates
  cluster-keyed names (`<cluster>-grafana-token`, per the resource-naming standard).
  The preflight would fail (or vacuously pass) against a current substrate.

Approach:
1. Config: add `cluster.endpointAllowlist []string` (CIDR-validated). Keep the existing
   production-must-be-private validation; extend validation: public + explicit allowlist
   entries must parse as CIDRs.
2. Cluster phase wiring: when provisioning network/cluster, append
   `TF_VAR_cluster_endpoint_public_access=<bool>` and, when public,
   `TF_VAR_cluster_endpoint_public_access_cidrs=<json list>` to the runner env
   (same idiom and placement as `TF_VAR_cluster_name`, `phases.go:372`).
   Confirm the exact landing-zone variable names against
   `landing-zone/components/aws/cluster/variables.tf` at implementation time.
3. Egress-IP autodetect: when `endpointPublicAccess: true` and the allowlist is empty,
   detect the operator's public egress IP at `init --apply` and use `<ip>/32`.
   Detection must be dry-run-aware (no network in dry-run; note the value it would
   fetch), use a boring well-known resolver endpoint with a timeout, and print what it
   detected and injected (the operator must see the CIDR that ends up on their control
   plane). Explicit allowlist always wins over detection.
4. Preflight names: derive the expected secret names from the config's cluster name
   (`<cluster>-grafana-token`, `<cluster>-managed-monitoring-endpoints` — verify the
   exact names landing-zone's managed-monitoring/cluster-addons components create today)
   instead of the stale `eks-*` literals.
5. `examples/rackctl.yaml` + README: document the new field, the autodetect behavior,
   and the division of labor (committed landing-zone tree is private-by-default; rackctl
   supplies posture + allowlist at apply time).
6. Tests: config validation cases (prod-public rejected, bad CIDR rejected), phase env
   injection asserted via the dry-run runner, autodetect fallback logic with a faked
   resolver, preflight name derivation.

Acceptance: `make test vet fmt` green; dry-run `rackctl init -c examples/rackctl.yaml`
shows the TF_VARs it would inject; landing-zone target 24 can remove its committed
flags immediately after this merges.
