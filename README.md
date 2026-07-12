# rackctl

**The day-0 installer for a [nanohype](https://github.com/nanohype) platform.**

`rackctl init` takes an operator from zero to a running, nanohype-shaped platform —
cloud, cluster, GitOps, controllers, and (optionally) the portal — then hands off to
the portal for day-2 operations. It is `kubefirst` for an agent-native platform.

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
rackctl doctor                            # check tools + cluster/ArgoCD health
rackctl destroy  -c rackctl.yaml --apply  # reverse-order teardown
rackctl upgrade  -c rackctl.yaml          # bump the catalog + operator
```

See [`examples/rackctl.yaml`](examples/rackctl.yaml) for the full config surface.

## The bootstrap pipeline (0 → running)

| # | Phase | What it does |
|---|-------|--------------|
| 0 | preflight | tools, caller identity, EC2 vCPU quota (files increases before provisioning) |
| 1 | acquire | clone `landing-zone` + `eks-agent-platform`, fork `eks-gitops` into the org |
| 2 | identity | `account.hcl` + versioned S3 tfstate backend |
| 3 | cluster | VPC → EKS control plane (strict ordering) → kubeconfig |
| 4 | gitops | secrets → ArgoCD + app-of-apps pointing at the org's `eks-gitops` fork |
| 5 | addons | `cluster-addons`, **auto IRSA-ARN writeback**, wait for ArgoCD convergence |
| 6 | platform | agent substrate (bedrock, cost-pipeline, kill-switch…), CRDs, operator |
| 7 | fleet *(opt-in)* | Crossplane + `eks-fleet` — clusters become `Cluster` CRs |
| 8 | portal *(opt-in)* | the day-2 operator UI |
| 9 | smoke *(opt-in)* | first-tenant end-to-end, enforcing the app-seam order |

Phases 0–6 are the core path (AWS-only, v1). On failure, completed phases are torn
down in reverse so a half-failed init never leaves billable resources.

### Footguns rackctl exists to kill

- **IRSA ARN substitution** — the `000000000000` placeholders in `eks-gitops/addons/*/values-<env>.yaml` (phase 5).
- **Service-quota deadlock** — fresh accounts cap ~32 vCPU (phase 0).
- **Operator OCI chicken-and-egg** — empty chart registry on a fresh org (phase 6).
- **Tenant app-seam ordering** — the 5-step `extraPolicyArns` dance (phase 9).
- **Teardown safety** — always clean up on failure (engine).

## Development

```sh
make build     # -> ./rackctl (version stamped from git)
make test      # go test -race ./...
make vet fmt
```

Layout:

```
cmd/            root · init · doctor · upgrade · destroy · version
internal/
  config/       rackctl.yaml schema + load/default/validate
  exec/         dry-run-aware tool runner (tofu/terragrunt/kubectl/helm/aws/gh)
  engine/       phase interface + pipeline + teardown + events
  phases/       the 10 bootstrap phases (footgun guards encoded)
  gitops/       IRSA account-id writeback
  tf/           terragrunt output parsing
  tui/          bubbletea progress view
  ui/           shared lipgloss styling
```

## Status & scope

- v1: **AWS only** (no `aks-gitops` catalog exists yet).
- CRD group: **`*.nanohype.dev`** (canonical).
- The CLI, config schema, phase engine, all commands, and the dry-run/TUI are wired;
  `--apply` executes against your account. Live end-to-end provisioning is validated
  by running it — the glue (repo acquisition, IRSA writeback, output parsing) is
  unit-tested.

## License

[Apache 2.0](LICENSE)
