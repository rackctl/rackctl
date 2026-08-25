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
| 7 | `resources-may-remain` | A phase failed **and** the rollback could not finish. | Needs a human. See [runbook.md](runbook.md) — *A teardown that stopped partway*. |
| 8 | `aborted` | The operator interrupted. | Whether the unwind finished depends on how far it got before a second interrupt. Verify with `rackctl check`, then `rackctl destroy` if needed. |

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

`clusterHealth` is `asserted`, `unreachable`, `wrong-cluster` or `not-asserted`. It is
load-bearing: an empty `platform` list means "no findings" only when `clusterHealth` is
`asserted` — otherwise it means nobody looked, and the two are the same document without it.
