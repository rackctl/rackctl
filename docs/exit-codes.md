# Exit codes

Every rackctl failure used to exit 1. A retry loop, a CI step or an agent could not then
tell "this config is invalid" from "the platform is half-built and billing" — outcomes that
call for opposite next moves.

The numbers are a contract. They are asserted by `TestExitCodes_AreStableAndDistinct` and
must not be renumbered.

| Code | Name | What it means | What to do |
|---:|---|---|---|
| 0 | — | Success. | — |
| 1 | `failure` | A failure with no more specific classification. | Read the message. |
| 2 | `config` | The config could not be loaded or did not validate, or a flag value was rejected. Nothing ran. | Fix `rackctl.yaml`. Nothing was created. |
| 3 | `preflight` | Preflight refused: this install would not succeed. From `rackctl check`, the pre-spend checks failed and nothing about a running platform did. | Clear the named checks. Nothing was spent. On `apply`, `--skip-preflight` overrides. |
| 4 | `declined` | A human refused the confirmation, or there was no human to ask. | Nothing was destroyed. Pass `--yes` for a scripted teardown. |
| 5 | `platform-left-standing` | A phase failed and the platform was deliberately left up — an optional phase, a convergence timeout, or a re-apply over a platform that was already standing. | The platform is usable. `rackctl check` says what is wrong. `rackctl destroy` to remove it. |
| 6 | `rolled-back` | A phase failed and the rollback ran, and every part of it held. | What this run created has been destroyed. Fix and re-run. |
| 7 | `resources-may-remain` | Something may still be billing and nobody can vouch for it: the rollback could not finish; a teardown (a rollback or `rackctl destroy`) ended with a backstop that failed or did not run; a `rackctl destroy` component failed; the run could not tell whether a platform already existed, so it rolled nothing back; or `--no-clean-on-failure` left a half-built platform in place. | Needs a human. See [runbook.md](runbook.md) — *A teardown that stopped partway*. |
| 8 | `aborted` | The operator interrupted (Ctrl-C, `SIGTERM`, or quitting `--tui`), and the unwind the run owed has finished — or nothing had been touched yet. | Verify with `rackctl check`, then `rackctl destroy` if needed. An interrupted run whose unwind did not finish exits `7` instead. |
| 9 | `refused` | A command refused before it touched anything: an `apply` stopped before any spend (a pin that does not resolve, a landing-zone checkout missing a live root, the preflight phase's tool or identity check), or a `rackctl destroy` refused before any destroy (an eks-fleet hub with live spokes). | Nothing changed. The message names what to fix. A destroy refusal says it repointed kubectl. |
| 10 | `unhealthy` | `rackctl check` ran and found the platform unhealthy, found kubectl pointed at a different cluster, or found the cluster in EKS and could not reach it from this shell. | Read the failed checks. For an unreachable or wrong cluster, run the `aws eks update-kubeconfig` line the check prints and re-run it. |

Codes `9` and `10` are additive: no existing code was renumbered. A caller that
tested `rackctl check` for exactly `1` now sees `3` or `10` for a check that ran and failed,
and `1` only for a check that could not run.

A backstop is a sweep that guards something terraform cannot see: the in-cluster reap, the
operator IAM roles, the Karpenter nodes and the orphaned EBS volumes. One that failed or did
not run can leave an instance billing or a role that stops the next teardown, so a rollback
that hits one is not complete and exits `7` rather than `6`, and a `rackctl destroy` whose
every component came down exits `7` rather than `0`. The message names each backstop that did
not hold. The unprovable-volume report is informational: it deletes nothing, so it never
decides the status, and a destroy whose guards all held exits `0` with its warning. A cluster
that no longer exists is a backstop that found nothing, so the re-run after a partial
teardown can finish clean.

## By command

**`rackctl apply`** classifies on what the engine recorded about the failure, never on the
message, which carries subprocess stderr. The rollback arm decides it: `6` only when the
rollback ran and held, `5` for a platform deliberately left standing, `7` when nobody can
vouch for what is left, `9` for a refusal before any spend on a run that found no platform
standing. An interrupt exits `8`, or `7` when the unwind it triggered did not finish; the
message starts with `interrupted:` either way.

**`rackctl plan`** builds nothing, so a failed plan exits `1` whatever its rollback would
have been, and its teardown is printed as what an apply `would roll back`.

**`rackctl check`** exits `0` when every check cleared — including a config with no
cluster in EKS yet, where only the pre-spend set can be true. It exits `10` when the
platform is unhealthy, kubectl points at a different cluster, or the cluster exists and
this shell cannot reach it; `3` when only pre-spend checks failed; `1` when it could not
run, or could not find out whether the cluster exists; `2` for a config that did not load.

**`rackctl destroy`** exits `0` when every component came down and every backstop held,
`7` when a component failed or a backstop did not hold, `9` when it refused before any
destroy, `4` when the confirmation was declined, and `8` when an interrupt arrived before
anything was destroyed. An interrupt once the teardown has begun leaves components standing
and exits `7`, with the message starting `interrupted:`.

The name is printed to stderr alongside the message, so a terminal and a `$?` carry the
same vocabulary:

```
resources-may-remain (exit 7): phase "substrate" failed: … (and the rollback did not complete: …)
```

## Machine-readable output

`rackctl check --output json` writes the report to stdout and every human line to stderr,
so it pipes without filtering:

```sh
rackctl check -c rackctl.yaml --output json | jq '.preflight[] | select(.status=="fail")'
```

```json
{
  "cluster": "development-platform",
  "environment": "development",
  "account": "111111111111",
  "region": "us-east-1",
  "clusterHealth": "asserted",
  "clusterExists": "present",
  "passed": false,
  "preflight": [{ "name": "vcpu quota", "status": "fail", "detail": "…" }],
  "platform": []
}
```

Each result's `status` is one of:

| Status | Meaning |
|---|---|
| `ok` | The check asked its question and the answer is good. |
| `warn` | Worth reading, and not a reason to refuse. This includes a read that did not answer: a check that could not find out says so here, naming what went unchecked, rather than reporting `ok`. |
| `fail` | The install would not succeed, or the platform is unhealthy. `passed` is false. |
| `skip` | The check does not apply to this config — a disabled feature asked nothing. `passed` treats it like `ok`, because nothing failed. |

A consumer that filtered on `status=="ok"` to mean "not a problem" should read `skip` the
same way. Checks for disabled features reported `ok` before `skip` existed; they now report
`skip`, so a filter that accepts only `ok` counts them as findings.

`clusterHealth` is `asserted`, `unreachable`, `wrong-cluster` or `not-asserted`. It is
load-bearing: an empty `platform` list means "no findings" only when `clusterHealth` is
`asserted` — otherwise it means nobody looked, and the two are the same document without it.

`unreachable` means kubectl reached no cluster, and `clusterExists` says which of its two
meanings applies: `absent` is no cluster yet, which passes on the pre-spend checks alone;
`present` is a cluster this shell cannot reach, which fails. `wrong-cluster` compares the
whole kubeconfig entry: for an EKS ARN the account and the region must match the config as
well as the name, because the same name in a sibling account is a different cluster.

`clusterExists` is `absent`, `present` or `unknown`: whether EKS has this config's cluster.
It is `present` for an asserted cluster, and absent from a report that stopped before it
could ask.

`not-asserted` is also what a check reports when kubectl reached no cluster and EKS could
not say whether one exists. That run finished, so it carries no `error`; `clusterExists` is
`unknown` and `passed` is false.

`not-asserted` is otherwise a check that stopped before it could finish — a required tool missing from
`PATH`, an identity that did not resolve, a config that did not load. The document is still
written, so a pipe into a parser never receives an empty stream: `passed` is `false`, the
result lists hold whatever ran (often nothing), and an `error` field carries the reason.
`error` is present only on such a run. The exit status is the one the failure already
carried — `2` for a config that did not load, `1` otherwise.

`passed` is false exactly when the exit status is non-zero.
