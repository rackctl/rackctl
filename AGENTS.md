# rackctl — agent entry point

You're an AI client, or the author of one. This file is the contract surface: what this
repo gives you, how to drive it, and how to add to it.

rackctl is the **composition**. The five nanohype repos are reusable substrate —
`landing-zone` (OpenTofu/Terragrunt AWS substrate), `eks-gitops` (ArgoCD catalog),
`eks-agent-platform` (operator and CRDs), `cloudgov` (governance CLI), `kx` (local cluster).
rackctl drives them into one running platform and hands off to the portal for day-2. It is
the only vantage point that sees a gap end to end, which makes it the place to **report**
one — never the place to absorb one. A defect in the substrate is fixed in the substrate.

## Driving it

```sh
rackctl plan    -c rackctl.yaml   # walk every phase read-only; makes read-only AWS calls
rackctl apply   -c rackctl.yaml   # provision; re-runnable by design
rackctl check   -c rackctl.yaml   # can this install succeed, and is a running one healthy
rackctl destroy -c rackctl.yaml --yes
```

**Machine-readable output.** `rackctl check --output json` writes the report to stdout and
every human line to stderr, so it pipes without filtering. The shape is published in
[`docs/exit-codes.md`](docs/exit-codes.md) and pinned by `TestCheckJSON_ShapeIsStable`;
fields are added, never renamed.

**Exit status classifies the outcome.** Nine codes, documented in
[`docs/exit-codes.md`](docs/exit-codes.md) and pinned by
`TestExitCodes_AreStableAndDistinct`. The three you must branch on: `5`
platform-left-standing (usable), `6` rolled-back (nothing remains), `7`
resources-may-remain (needs a human). The name is printed alongside the message, so a
terminal and a `$?` carry the same vocabulary.

**A dry-run queries for real.** `plan` and `destroy --dry-run` make read-only AWS calls
deliberately: the sweeps that delete resources outside Terraform's state enumerate live and
print exactly what they would select. A dry-run that queried nothing could only restate its
own filter back.

## The shape of the repo

```
cmd/            cobra wiring: plan · apply · check · destroy · version, exit codes
internal/
  config/       rackctl.yaml schema + load/default/validate; retired-field refusal
  exec/         the ONLY process spawn point — Run (mutates) / Capture (reads) /
                Query (reads, runs even in dry-run); ceilings and per-call identity
  engine/       Phase interface + ordered pipeline + rollback discipline + events
  phases/       the ten bootstrap phases; footgun guards encoded
  preflight/    "would this install succeed?" — gates apply, surfaced by check
  doctor/       day-2 health checks
  reap/         sweeps what Terraform does not own; ownership proved by tags
  gitops/       catalog account-id writeback
  awsid/        assume-role session, cached and re-assumed before expiry
  tf/           terragrunt output parsing
  tui/          bubbletea progress view
  ui/           shared lipgloss styling
scripts/        the gate suite (see below) and the installer
docs/           runbook, exit codes
```

Every subprocess in the repo goes through `internal/exec` — `grep -rn '"os/exec"'` returns
that package and nothing else. If you are adding a call to an external tool, it goes there.

## Adding a phase

1. Implement `engine.Phase` (`internal/engine/phase.go`): `ID`, `Title`, `Optional`,
   `Enabled`, `Run`, `Teardown`. Embed `base` from `internal/phases/phases.go` for
   everything but `Run` and `Teardown`.
2. Register it in `phases.All()`. Order is load-bearing and terragrunt's dependency graph
   does not express it — the slice plus the phase boundary is what sequences the tree.
3. If the phase applies a landing-zone component, add it to `CoreComponents` and give it a
   `componentEnv` case. Derive from `CoreComponents`; never restate the list.
4. A failure that must NOT destroy the cloud returns `engine.NoRollbackError`. A failure to
   PROVISION may roll back; a failure to CONVERGE may not.
5. Tests drive the production `exec.Runner` against fake executables on `PATH` — see
   `internal/phases/kubeconfig_test.go`. That exercises the argv you actually build.

## Adding a config field

`internal/config/config.go`. Validate it in `Validate()` — a value that reaches a policy
document, a Kubernetes object name or a Helm release name is validated at the boundary, not
trusted at the point of use. A field that is removed goes in `internal/config/retired.go` so
a config still setting it is refused by name rather than silently ignored.

Defaults are applied **field by field**, never by replacing a struct: replacing one wipes a
sibling the operator set deliberately. A boolean that defaults to true is a `*bool` with an
accessor, because a plain bool cannot tell "explicitly false" from "omitted".

## The gate suite

`make gates` and `make cover`. Every gate is discovered rather than listed, carries positive
controls that run on **every invocation**, and must demonstrate both a rejection and a clean
fixture accepted. A gate shipped without controls fails the suite; so does discovering zero
gates.

| Gate | Holds |
|---|---|
| `scripts/coverage.sh` | the org coverage floor, and 100% on every function that decides what gets destroyed |
| `scripts/pins.py` | every version pinned in the tree is watched by a real [`.github/renovate.json`](.github/renovate.json) manager |
| `scripts/prose.py` | the enforceable subset of `documentation-voice` |
| `scripts/floor.py` | the anti-vacuity floor: it FEEDS each gate above a known-bad input and reads the exit status, consulting nothing the gate prints about itself |

If you add a gate, it needs a known-good and known-bad fixture pair registered in
`scripts/floor.py`. A gate with no pair fails the floor — a gate is proven by being fed a
bad input and watched to reject it, and nothing it prints about itself is evidence. It
also needs `--controls-only` with controls that fail without the behaviour they guard, and
a control counts as landed only when the fixture changed, the marker it claimed to plant is
present, and it was not already there.

## Conventions

- Tabs for Go, 2-space YAML and JSON. `gofmt`, `go vet` and `golangci-lint` all gate merges.
- Errors wrap with `%w` and name the tool. Every external call carries a deadline.
- Prose is graded against `nanohype/standards/documentation-voice.json`: no verification
  tallies, no internal issue or ledger references, no line-number citations, no session
  narration. `scripts/prose.py` holds the enforceable half; the rest is review.
- Estate values stay out. An operator supplies their own account, region and org — a value
  from one real estate presented as the product's shape is a defect in prose exactly as in
  code.

## Pointers

- [`README.md`](README.md) — what rackctl is, the pipeline, and the footguns it exists to kill
- [`docs/runbook.md`](docs/runbook.md) — what to do when an install, check or teardown goes wrong
- [`docs/exit-codes.md`](docs/exit-codes.md) — the exit-status and JSON-report contracts
- [`examples/rackctl.yaml`](examples/rackctl.yaml) — the full config surface, annotated
- [`.env.example`](.env.example) — every environment variable rackctl reads
