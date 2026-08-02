# rackctl

**The day-0 installer for a [nanohype](https://github.com/nanohype) platform.**

`rackctl apply` takes an operator from zero to a running, nanohype-shaped platform —
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
rackctl plan     -c rackctl.yaml          # what a provision would do (read-only)
rackctl apply    -c rackctl.yaml          # provision for real
rackctl apply    -c rackctl.yaml --tui    # interactive progress view
rackctl check    -c rackctl.yaml          # can this install succeed, and is a running one healthy
rackctl destroy  -c rackctl.yaml --apply  # reverse-order teardown
rackctl destroy  -c rackctl.yaml --apply --force-buckets  # ...also emptying non-emptyable buckets
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
| 6 | platform | applies the agent-platform AWS substrate, then **waits for** the catalog-delivered operator — three CRD groups Established + the operator Deployment Available. It does not install the operator; the GitOps catalog owns that. |
| 7 | fleet *(opt-in)* | Crossplane + `eks-fleet` — clusters become `Cluster` CRs |
| 8 | portal *(opt-in)* | the day-2 operator UI |
| 9 | smoke *(opt-in)* | vends the first tenant from `eks-agent-platform/charts/tenant` and waits for `status.phase: Ready` |

Phases 0–6 are the core path (AWS-only, v1).

On failure the completed phases are torn down in reverse, so a half-failed init does not
strand a VPC and an EKS control plane. That rollback is deliberately **not** unconditional
— three cases leave the platform standing instead, because destroying it would cost more
than it saves:

- an **optional** phase (7–9) failing — the platform underneath is already provisioned and
  nothing before it depended on the optional phase
- **ArgoCD failing to install or converge** in phase 5 — the cloud is built and the cluster
  is the only surface the failure can be diagnosed on
- a **refusal issued before anything ran**, such as `--force-buckets` against druid outside
  development

Each prints why it stopped and leaves `rackctl destroy` as the explicit next step. A phase
that returns `engine.NoRollbackError` is opting into this; anything else rolls back.

### Footguns rackctl exists to kill

- **Catalog account-id plumbing** — the account id must never be committed to the (public) catalog. Almost every addon binds its IAM role through EKS Pod Identity, which needs no ARN in the values at all; the one ApplicationSet that does need a role ARN reads it from an annotation `cluster-bootstrap` stamps on the ArgoCD cluster Secret (phase 5). rackctl's writeback over `eks-gitops/addons/**/values-<env>.yaml` (phase 4) substitutes the account id into any values file that carries a `000000000000` placeholder — against this catalog there are none, and the phase says so rather than reporting a silent zero.
- **Missing live roots** — landing-zone's live tree is authored per environment, and not every component has a leaf in every one (`agent-iam` is development-only today, and rackctl applies it **by default**). The acquire phase stats every component this config will apply and fails naming the component, the environment, the exact missing path and the knob that turns it off (phase 1) — because the same failure discovered in phase 4 costs a VPC, an EKS control plane, and the teardown of both.
- **EKS endpoint posture** — landing-zone's committed tree is private-by-default and fail-closed: a public API endpoint with no allow-list is rejected at plan time (no `0.0.0.0/0` fallback). rackctl owns the fragile per-run input. `cluster.endpointPublicAccess` and `cluster.endpointAllowlist` ride `TF_VAR_cluster_endpoint_public_access(_cidrs)` into the cluster component (phase 3); a public endpoint with an empty allow-list auto-scopes to the operator's detected egress IP (`<ip>/32`), printed before it lands, and an explicit allow-list always wins.
- **Network topology, create or adopt** — `cluster.network.mode` defaults to `create`: the hub mints its own VPC, subnets, endpoints, egress and ELB role tags. `adopt` joins a VPC someone else owns (a shared VPC in the account, or one shared over AWS RAM) and builds **nothing** — the VPC, subnets, CIDR and AZs are resolved from the `adopt*` fields and re-exported through the same outputs, so the cluster wires identically either way. rackctl validates the shape up front (ids well-formed, no subnet in both tiers, at least three distinct private subnets for the zone spread landing-zone asserts) and sends `nat_gateways=1` / `enable_flow_logs=false` to neutralize create-mode values the staging and production leaves pin, which landing-zone rejects under adopt. Under `create`, a plain literal-CIDR VPC with local NAT is the default; the `cluster.network` levers `ipamPoolId` / `ipamNetmaskLength` / `transitGatewayId` / `centralizedEgress` opt it into the org's IPAM / transit-gateway topology, riding `TF_VAR_ipam_pool_id` / `TF_VAR_ipam_netmask_length` / `TF_VAR_transit_gateway_id` / `TF_VAR_centralized_egress` onto the network component (phase 3), layered over the generic committed tree like the endpoint posture. rackctl mirrors landing-zone's own preconditions (an IPAM pool excludes a non-default `vpcCidr` and needs a 16–20 netmask; a transit gateway needs the IPAM CIDR; centralized egress needs the transit gateway), so a bad combination fails in `rackctl plan` in a second rather than ~20 minutes into a `tofu apply`. A dry-run prints each lever it would inject before anything is applied.
- **Observability tier vs. its substrate** — `cluster-bootstrap` takes `observability_tier` and `enable_managed_monitoring` as *independent* variables with nothing relating them, and every committed leaf pins the tier to `full`. So turning monitoring "off" used to set the flag false and leave the label at `full`: a cluster advertising the full tier with no AMP behind it, whose full-tier OTel gateway then sat in `CreateContainerConfigError` waiting on an `AMP_REMOTE_WRITE_URL` Secret that external-secrets could not sync, because the Secrets Manager entry behind it is written by `managed-monitoring` and `managed-monitoring` had not run. The real invariant is cross-root and unexpressible as a Terraform validation — `tier: full` requires the `managed-monitoring` component to have *applied*, and no variable block can see whether another root ran. rackctl can, so one field (`observability.tier`, default `full`) decides all three: whether the component is applied, the label, and the Grafana flag. The broken combination is no longer expressible.
- **Service-quota deadlock** — fresh accounts cap ~32 vCPU (phase 0).
- **Chart-registry chicken-and-egg** — `oci://ghcr.io/nanohype/charts/portal` is not published on a fresh org, so the portal phase falls back to the local chart in the cloned repo (phase 8). The agent *operator* is not installed by rackctl at all: the GitOps catalog owns it, and phase 6 waits for it to arrive. Phase 6 does apply the operator's AWS **substrate**, which the catalog cannot create.
- **Placeholders that install cleanly and do nothing** — `eks-fleet` ships `config/bootstrap/providers.yaml` with a literal `<FLEET_ACCOUNT_ID>`, and its stand-up doc substitutes the real ARN by hand at apply time. Applied unrendered it installs a provider that never receives credentials **while still reporting Healthy** — an error would be kinder. `controlPlane.fleetHubRoleArn` is therefore required whenever `eksFleet` is on, validated as an IAM role ARN, and substituted into the annotation with its indentation preserved; if the annotation is missing or the placeholder survives, the phase refuses rather than installing something inert.
- **A credential nothing asked for** — setting `org.gitops.tenantsRepo` sends `TF_VAR_tenants_repo_url`, which arms `cluster-bootstrap`'s `provider "github"`. That provider authenticates from `GITHUB_TOKEN` and nothing else, so without one the apply 401s in phase 5 with the VPC and cluster already built. The documented setup path made it worse: `gh auth login` stores the credential in gh's keyring and exports nothing. rackctl now honours an exported `GITHUB_TOKEN`, otherwise asks gh for the token it already holds, and refuses before the apply if neither exists. Preflight asks the same question before a dollar is spent.
- **Tenant vending seam** — a `Platform` never gets a `Ready` *condition*. The operator reports readiness as `status.phase: Ready` (the CRD's printcolumn reads `.status.phase`; the only conditions it writes are `Suspended` / `ModelAccessScoped` / `NamespaceReady` / `VClusterReady`), so `kubectl wait --for=condition=Ready platform/<name>` blocks for its whole timeout against a tenant that is already up. And `charts/tenant` plants its CRs in `controlPlaneNamespace` (default `eks-agent-platform`), not in the Helm release namespace, so a namespace-blind wait looks in the wrong place. Phase 9 pins both: it drives the chart on its own value paths (`platform.name` / `platform.tenant` / `platform.persona`), passes the same namespace as `--namespace` *and* `--set controlPlaneNamespace=` so release and objects can never diverge, and waits on `--for=jsonpath={.status.phase}=Ready -n eks-agent-platform`. It proves the vending + identity path only — `charts/tenant` cannot express `spec.datastores`, `spec.identity.capabilities` or `spec.identity.directSecretReads`, which is where a tenant's AWS grants now come from.
- **Per-component TF_VAR scoping** — rackctl owns the fragile per-run inputs and layers them onto a committed tree that stays generic, and an ambient `TF_VAR_*` **overrides** whatever a terragrunt leaf pinned for that environment. So a variable set on the shared runner is not an input to one component, it is an input to every component applied after it: `TF_VAR_cluster_name` used to reach `secrets`, `agent-iam`, `managed-monitoring`, `dns`, `cluster-addons` and `cluster-bootstrap`, whose own envcommon was simultaneously handing them the *resolved* cluster name, and the leaked base name won silently. Each variable is now scoped to the single invocation that declares it (`componentEnv`), restored afterwards, and shared by the apply and destroy paths so the two cannot drift. A variable is scoped or it is a global — there is no third thing, and `cmd/tgenv.go` holds the genuine globals.
- **Teardown safety, in both directions** — a failed `--apply` destroys the completed phases in reverse (`terragrunt destroy` per component), so no stranded VPC/EKS. The harder half is knowing when *not* to: a rollback that runs when it should not is the more expensive bug. rackctl refuses to sweep a platform this run did not create, refuses to destroy an `eks-fleet` hub while it still has spokes (that orphans real clusters in other accounts), and treats an optional phase's failure or a phase-5 convergence failure as "leave it standing" rather than "demolish it".
- **`--force-buckets` is two acts, and sometimes zero** — `force_destroy` has no effect until an apply has landed it in state, so injecting it only on the destroy path fails on `BucketNotEmpty`. The flag is applied first, then the teardown runs. druid is covered: the permitting apply clears its tenant Aurora's `deletion_protection` in the same act that lands `force_destroy`, so act 2 reaches the per-tenant deepstorage buckets and the DB cluster together. Without the flag that destroy deletes the deepstorage buckets (no versioning, no expiry, only copy) and *then* fails on `DeleteDBCluster` — segments gone, Aurora standing, sweep halted with the cluster, VPC and NAT gateways still billing.

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
  preflight/    "would this install succeed?" checks; gates `rackctl apply`, surfaced by `check`
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
  reverse **unless the phase opted out** (see above). Live end-to-end provisioning is
  validated by running it — the engine ordering, both rollback and no-rollback paths,
  the live-root guard, per-component TF_VAR scoping, the tenant-vending argv, the
  apply/destroy component sets, output parsing, and config validation are covered by tests.
  Destructive-path fixes are mutation-tested: the bug is reintroduced and the guarding
  test must go red, because a test that cannot fail guards nothing.

## License

[Apache 2.0](LICENSE)
