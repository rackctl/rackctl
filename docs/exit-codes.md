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
| 3 | `preflight` | Preflight refused: this install would not succeed. | Clear the named checks. Nothing was spent. `--skip-preflight` overrides. |
| 4 | `declined` | A human refused the confirmation, or there was no human to ask. | Nothing was destroyed. Pass `--yes` for a scripted teardown. |
| 5 | `platform-left-standing` | A phase failed and the platform was deliberately left up — an optional phase, a convergence timeout, or a run that did not build what it found. | The platform is usable. `rackctl check` says what is wrong. `rackctl destroy` to remove it. |
| 6 | `rolled-back` | A phase failed and the rollback ran to completion. | What this run created has been destroyed. Fix and re-run. |
| 7 | `resources-may-remain` | A phase failed **and** the rollback could not finish, or a teardown (a rollback or `rackctl destroy`) ended with a backstop that failed or did not run. | Needs a human. See [runbook.md](runbook.md) — *A teardown that stopped partway*. |
| 8 | `aborted` | The operator interrupted. | Whether the unwind finished depends on how far it got before a second interrupt. Verify with `rackctl check`, then `rackctl destroy` if needed. |

A backstop is a sweep that guards something terraform cannot see: the in-cluster reap, the
operator IAM roles, the Karpenter nodes and the orphaned EBS volumes. One that failed or did
not run can leave an instance billing or a role that stops the next teardown, so a rollback
that hits one is not complete and exits `7` rather than `6`, and a `rackctl destroy` whose
every component came down exits `7` rather than `0`. The message names each backstop that did
not hold. The unprovable-volume report is informational: it deletes nothing, so it never
decides the status, and a destroy whose guards all held exits `0` with its warning. A cluster
that no longer exists is a backstop that found nothing, so the re-run after a partial
teardown can finish clean.

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

`not-asserted` is a check that stopped before it could finish — a required tool missing from
`PATH`, an identity that did not resolve, a config that did not load. The document is still
written, so a pipe into a parser never receives an empty stream: `passed` is `false`, the
result lists hold whatever ran (often nothing), and an `error` field carries the reason.
`error` is present only on such a run. The exit status is the one the failure already
carried — `2` for a config that did not load, `1` otherwise.
