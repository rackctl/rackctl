# rackctl

**The day-0 installer for a [nanohype](https://github.com/nanohype) platform.**

`rackctl init` takes an operator from zero to a running, nanohype-shaped platform —
cloud, cluster, GitOps, controllers, and (optionally) the portal — then hands off to
the portal for day-2 operations.

rackctl is an **orchestrator, not a rewrite**: it drives the existing nanohype repos
(`landing-zone` Terragrunt, `eks-gitops` ArgoCD catalog, `eks-agent-platform` operator)
and automates the manual glue documented in `landing-zone/docs/first-deploy-aws.md` —
especially the footguns that strand a human today.

## Install

```sh
curl -fsSL rackctl.com/install | sh
```

## Usage

```sh
rackctl init     -c rackctl.yaml          # dry-run plan (no cloud changes)
rackctl init     -c rackctl.yaml --apply  # provision for real
rackctl init     -c rackctl.yaml --tui    # interactive progress view
rackctl preflight -c rackctl.yaml         # "would this install succeed?" (init --apply runs it as a gate)
rackctl doctor                            # check tools + cluster/ArgoCD health
rackctl destroy  -c rackctl.yaml --apply  # reverse-order teardown
rackctl upgrade  -c rackctl.yaml          # bump the catalog + operator
```

See [`examples/rackctl.yaml`](examples/rackctl.yaml) for the full config surface.

## The bootstrap pipeline (0 → running)

| # | Phase | What it does |
|---|-------|--------------|
| 0 | preflight | tools, caller identity, EC2 vCPU quota (files increases before provisioning) |
| 1 | acquire | clone `landing-zone` + `eks-agent-platform`, fork `eks-gitops` into the org, then verify a live root exists for every component this config will apply |
| 2 | identity | versioned S3 tfstate backend |
| 3 | cluster | VPC → EKS control plane (strict ordering) → kubeconfig |
| 4 | substrate | `secrets` → `agent-iam` → `managed-monitoring` → `observability` → `dns` → `model-import` → `cluster-addons` — every AWS resource ArgoCD will consume, built before ArgoCD exists |
| 5 | gitops | `cluster-bootstrap`: ArgoCD + app-of-apps pointing at the org's `eks-gitops` fork, then wait for convergence |
| 6 | platform | **waits for** the catalog-delivered agent operator — three CRD groups Established + the operator Deployment Available. Installs nothing. |
| 7 | fleet *(opt-in)* | Crossplane + `eks-fleet` — clusters become `Cluster` CRs |
| 8 | portal *(opt-in)* | the day-2 operator UI |
| 9 | smoke *(opt-in)* | vends the first tenant from `eks-agent-platform/charts/tenant` and waits for `status.phase: Ready` |

Phases 0–6 are the core path (AWS-only, v1). On failure, completed phases are torn
down in reverse so a half-failed init never leaves billable resources.

### Footguns rackctl exists to kill

- **Catalog account-id plumbing** — the account id must never be committed to the (public) catalog. Almost every addon binds its IAM role through EKS Pod Identity, which needs no ARN in the values at all; the one ApplicationSet that does need a role ARN reads it from an annotation `cluster-bootstrap` stamps on the ArgoCD cluster Secret (phase 5). rackctl's writeback over `eks-gitops/addons/**/values-<env>.yaml` (phase 4) substitutes the account id into any values file that carries a `000000000000` placeholder — against this catalog there are none, and the phase says so rather than reporting a silent zero.
- **Missing live roots** — landing-zone's live tree is authored per environment, and not every component has a leaf in every one (`agent-iam` is development-only today, and rackctl applies it **by default**). The acquire phase stats every component this config will apply and fails naming the component, the environment, the exact missing path and the knob that turns it off (phase 1) — because the same failure discovered in phase 4 costs a VPC, an EKS control plane, and the teardown of both.
- **EKS endpoint posture** — landing-zone's committed tree is private-by-default and fail-closed: a public API endpoint with no allow-list is rejected at plan time (no `0.0.0.0/0` fallback). rackctl owns the fragile per-run input. `cluster.endpointPublicAccess` and `cluster.endpointAllowlist` ride `TF_VAR_cluster_endpoint_public_access(_cidrs)` into the cluster component (phase 3); a public endpoint with an empty allow-list auto-scopes to the operator's detected egress IP (`<ip>/32`), printed before it lands, and an explicit allow-list always wins.
- **Create-mode network topology** — day-0 bootstrap is `create` mode only: the hub mints its own VPC (adopt is a spoke/`eks-fleet` concern). A plain literal-CIDR VPC with local NAT is the default; the `cluster.network` levers `ipamPoolId` / `ipamNetmaskLength` / `transitGatewayId` / `centralizedEgress` opt it into the org's IPAM / transit-gateway topology, riding `TF_VAR_ipam_pool_id` / `TF_VAR_ipam_netmask_length` / `TF_VAR_transit_gateway_id` / `TF_VAR_centralized_egress` onto the network component (phase 3), layered over the generic committed tree like the endpoint posture. rackctl mirrors landing-zone's own preconditions (an IPAM pool excludes a non-default `vpcCidr` and needs a 16–20 netmask; a transit gateway needs the IPAM CIDR; centralized egress needs the transit gateway), so a bad combination fails in `rackctl init` validation in a second rather than ~20 minutes into a `tofu apply`. A dry-run prints each lever it would inject before anything is applied.
- **Observability tier vs. its substrate** — `cluster-bootstrap` takes `observability_tier` and `enable_managed_monitoring` as *independent* variables with nothing relating them, and every committed leaf pins the tier to `full`. So turning monitoring "off" used to set the flag false and leave the label at `full`: a cluster advertising the full tier with no AMP behind it, whose full-tier OTel gateway then sat in `CreateContainerConfigError` waiting on an `AMP_REMOTE_WRITE_URL` Secret that external-secrets could not sync, because the Secrets Manager entry behind it is written by `managed-monitoring` and `managed-monitoring` had not run. The real invariant is cross-root and unexpressible as a Terraform validation — `tier: full` requires the `managed-monitoring` component to have *applied*, and no variable block can see whether another root ran. rackctl can, so one field (`observability.tier`, default `full`) decides all three: whether the component is applied, the label, and the Grafana flag. The broken combination is no longer expressible.
- **Service-quota deadlock** — fresh accounts cap ~32 vCPU (phase 0).
- **Chart-registry chicken-and-egg** — `oci://ghcr.io/nanohype/charts/portal` is not published on a fresh org, so the portal phase falls back to the local chart in the cloned repo (phase 8). The agent operator is no longer installed by rackctl at all: the GitOps catalog owns it, and phase 6 only verifies it arrived.
- **Tenant vending seam** — a `Platform` never gets a `Ready` *condition*. The operator reports readiness as `status.phase: Ready` (the CRD's printcolumn reads `.status.phase`; the only conditions it writes are `Suspended` / `ModelAccessScoped` / `NamespaceReady` / `VClusterReady`), so `kubectl wait --for=condition=Ready platform/<name>` blocks for its whole timeout against a tenant that is already up. And `charts/tenant` plants its CRs in `controlPlaneNamespace` (default `eks-agent-platform`), not in the Helm release namespace, so a namespace-blind wait looks in the wrong place. Phase 9 pins both: it drives the chart on its own value paths (`platform.name` / `platform.tenant` / `platform.persona`), passes the same namespace as `--namespace` *and* `--set controlPlaneNamespace=` so release and objects can never diverge, and waits on `--for=jsonpath={.status.phase}=Ready -n eks-agent-platform`. It proves the vending + identity path only — `charts/tenant` cannot express `spec.datastores`, `spec.identity.capabilities` or `spec.identity.directSecretReads`, which is where a tenant's AWS grants now come from.
- **Per-component TF_VAR scoping** — rackctl owns the fragile per-run inputs and layers them onto a committed tree that stays generic, and an ambient `TF_VAR_*` **overrides** whatever a terragrunt leaf pinned for that environment. So a variable set on the shared runner is not an input to one component, it is an input to every component applied after it: `TF_VAR_cluster_name` used to reach `secrets`, `agent-iam`, `managed-monitoring`, `dns`, `cluster-addons` and `cluster-bootstrap`, whose own envcommon was simultaneously handing them the *resolved* cluster name, and the leaked base name won silently. Each variable is now scoped to the single invocation that declares it (`componentEnv`), restored afterwards, and shared by the apply and destroy paths so the two cannot drift. A variable is scoped or it is a global — there is no third thing, and `cmd/tgenv.go` holds the genuine globals.
- **Teardown safety** — a failed `--apply` destroys the completed phases in reverse (`terragrunt destroy` per component); no stranded VPC/EKS (engine + per-phase `Teardown`).

## Development

```sh
make build     # -> ./rackctl (version stamped from git)
make test      # go test -race ./...
make vet fmt
```

Layout:

```
cmd/            root · init · preflight · doctor · upgrade · destroy · version
internal/
  config/       rackctl.yaml schema + load/default/validate
  exec/         dry-run-aware tool runner (tofu/terragrunt/kubectl/helm/aws/gh)
  engine/       phase interface + pipeline + teardown + events
  phases/       the 10 bootstrap phases (footgun guards encoded)
  preflight/    "would this install succeed?" checks; gates `init --apply`
  doctor/       day-2 health checks (tools, cluster, ArgoCD, wedged finalizers)
  reap/         sweeps what Terraform does not own (operator IAM roles, PVCs, nodes, volumes)
  gitops/       catalog account-id writeback
  tf/           terragrunt output parsing
  tui/          bubbletea progress view
  ui/           shared lipgloss styling
```

## Status & scope

- v1: **AWS only** (no `aks-gitops` catalog exists yet).
- CRD group: **`*.nanohype.dev`** (canonical).
- The CLI, config schema, phase engine, all commands, and the dry-run/TUI are wired.
  `--apply` executes against your account; on failure, completed phases tear down in
  reverse. Live end-to-end provisioning is validated by running it — the engine
  ordering + reverse-teardown, the live-root guard, the tenant-vending argv, the
  never-destroy set, output parsing, and config validation are covered by tests.

## License

[Apache 2.0](LICENSE)
