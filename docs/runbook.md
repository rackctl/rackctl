# rackctl runbook

What to do when an install, a health check or a teardown goes wrong. `rackctl apply`
provisions a VPC, an EKS control plane and a full AWS substrate, so a failure partway
leaves real, billable resources behind — the question is always which ones, and whether
the tool already removed them.

## Before you run anything destructive

`rackctl apply` and `rackctl destroy` print a banner first:

```
rackctl destroy — acme · 123456789012 · acme-production · us-east-1 · production
                  ↑ org   ↑ account      ↑ profile         ↑ region   ↑ environment
```

Confirm the **account** and the **profile** in that line before you proceed. The region is
shared by every account you have and the org name says nothing about which credentials are
in scope, so those two are the only fields that identify the cloud about to change.

Under the banner, `plan`, `apply` and `check` list every value the config left out and the
default used, such as `cluster.endpointPublicAccess: true`. A default you did not mean is
fixed by setting the field. `cloud.region` and `environment` have no default: a config
without them exits `2` naming the field, and so does a node-group size set to `0`, which is
refused rather than replaced.

`rackctl destroy` then asks you to type the cluster name. That is deliberately worse
ergonomics than a y/n: the failure this guards is a reflex failure, and the name is the one
thing you cannot type correctly while thinking about a different cluster.

## Did the rollback run?

On a failed phase, `rackctl apply` tears down the phases it completed, in reverse — but
**not always**, and the distinction decides your next command.

| What you see | What happened | Exit | What to do |
|---|---|---:|---|
| `failure detected — rolling back provisioned resources` | The rollback ran. Cloud created by this run is being destroyed. | `6`, or `7` | Wait for it to finish. If it reports `the rollback did not complete`, see *A teardown that stopped partway*. |
| `failure detected — plan only: would roll back provisioned resources` | A `rackctl plan` failed. Nothing was created and nothing is destroyed; this is the walk an apply would take. | `1` | Fix the failure and plan again. |
| `existing platform detected — plan only: apply would NOT roll back a failure over it`, or `could not determine whether a platform already exists — plan only: apply would NOT roll back` | A `rackctl plan` asked EKS for real and found an apply would not roll back. Nothing was created. | `1` | Expected on a re-apply. On a first install, fix credentials or connectivity before applying. |
| `existing platform detected — a failure will NOT roll it back` | The platform was already up before this run. Nothing was destroyed. | `5` | Fix the failure and re-run `rackctl apply`. It is re-runnable. |
| `could not determine whether a platform already exists — rollback is DISABLED` | The EKS list call failed, so rackctl could not tell a fresh install from a re-apply, and fails closed. Nothing was destroyed, and a half-built first install may be standing. | `7` | Fix credentials or connectivity, then `rackctl check`; re-run, or `rackctl destroy` what was built. |
| `--no-clean-on-failure: leaving what this run built in place` | The rollback was turned off. What the run built before the failure is still there. | `7` | `rackctl destroy` to remove it, or fix and re-run. |
| `ArgoCD applications did not all reach Healthy`, `never established`, `agent operator never became Available` or `republishing cluster-bootstrap for eval-runtime`, each with `The cloud IS provisioned` | Phase 5 or phase 6 failed to converge. The cloud is built and left standing. | `5` | `rackctl check` to see what has not settled; re-run `rackctl apply`, or `rackctl destroy` if you want it gone. |
| `this is an optional phase and the platform underneath it is provisioned` | Phase 7, 8 or 9 failed. The core platform is up and the run continued. | `5` | `rackctl check` to see what is wrong. The platform is usable. |
| `the core platform is up; optional phases failed` | Same, reported at the end. The exit status is still non-zero. | `5` | As above. |

A refusal before anything is spent — a pin that does not resolve, a landing-zone checkout
missing a live root, the preflight phase's tool or identity check — exits `9`, on a first
install and on a re-apply alike: nothing was touched, so there is nothing to roll back or
leave standing. That holds when the run could not tell whether a platform already exists,
too, and an interrupt that arrives before anything was touched exits `8`.

A phase that returns `NoRollbackError` never rolls back — a workload that has not converged
is not a reason to destroy the cloud it runs on.

## Common failures

### The run stops at preflight

Preflight refuses before anything is spent. Each result names its own remedy. The ones that
recur:

- **`aws identity`** — the profile in scope belongs to a different account than
  `cloud.accountId`. Provisioning would build a complete, healthy platform in someone
  else's account.
- **`session lifetime`** — under an hour of credentials left, or
  `cloud.assumeRole.durationSeconds` set below an hour. A token that lapses mid-phase leaves
  a half-applied run that cannot roll itself back, because the rollback needs the same
  credentials. Run `aws sso login --profile <profile>`, or raise `durationSeconds`.
  rackctl assumes the role anew before every mutating call, so each call starts with the
  role's whole duration. A role assumed from an SSO profile is chained, and STS caps a
  chained session at 3600s and refuses a longer one; the check warns when that applies,
  because one call that runs past the hour outlives its credentials.
- **`bucket names`** — an S3 name this run would create is already taken. S3 names are
  globally unique across every AWS account, so this is unrecoverable by retry: rename the
  cluster.
- **`vcpu quota`** — a fresh account caps at ~32 vCPU. With `quotas.autoRequest` on,
  `rackctl apply` files the increase before the gate, or finds the request already open,
  and exits `9` refused naming the request id and its case id. It files only once the
  credentials match `cloud.accountId`; a mismatch files nothing and the gate's `aws
  identity` check refuses it. AWS takes minutes to days to
  grant it; re-run `rackctl apply` once it has. `rackctl check` names the open request too,
  and with `autoRequest` off it prints the `aws service-quotas` command to file it yourself.
- **`github token`** — `org.gitops.tenantsRepo` is set, which arms cluster-bootstrap's
  GitHub provider, and that provider authenticates from `GITHUB_TOKEN` and nothing else.
  `gh auth login` stores the credential in gh's keyring and exports nothing. Export
  `GITHUB_TOKEN`, or let rackctl ask gh for the token it already holds.

`--skip-preflight` exists because a check can be wrong. It has to be asked for.

### A terragrunt apply fails

The tool's own stderr is in the error. Read it before anything else — `AccessDenied`, a
missing live root and a genuine resource conflict all look identical without it.

If the failure is a missing live root, the acquire phase should have caught it in phase 1
and named the component, the environment, the exact missing path and the knob that turns it
off. Reaching it in phase 4 instead means the component set changed after acquire ran.

### The catalog will not converge

Phase 5 waits up to 30 minutes for every ArgoCD Application to report Healthy, requiring two
consecutive samples at an unchanged count. On expiry it returns `NoRollbackError`, so the
cloud stays standing, and says which of three things it saw last:

- **the Application list was never readable** — the API, not the catalog: credentials, RBAC
  or reachability.
- **app-of-apps generated no child Applications** — the catalog source, or the credential to
  read it.
- **N of M not Healthy**, by name — the catalog is there and something in it has not settled.

Some workloads converge slowly by design: opencost crashloops until its metrics reach AMP,
which takes minutes of scraping. A wait that expires with almost everything Healthy is not a
failed install.

```sh
kubectl -n argocd get applications
rackctl check -c rackctl.yaml     # names what is wrong, per check
```

### An interrupt

The first Ctrl-C cancels the run's context. A running terragrunt or tofu gets one SIGINT,
its graceful stop, and up to two minutes to write its state and release the state lock
before it is killed; rackctl prints `waiting up to 2m0s for <tool> to release state` while
it does. Then the rollback runs. **The second Ctrl-C forces exit**: rackctl kills every tool
it started, process group and all, starts nothing more, and exits `7`, because the unwind
did not finish and what the run built may remain. Use it only if the unwind itself has hung.
A second Ctrl-C before any tool that can change the cloud started, at the `destroy` prompt or
during `check` or `plan`, exits `8` instead and says nothing was touched.

Under `--tui`, `q` does the same, and the footer says so before you press it. The first `q`
stops the run and keeps the view up while the rollback runs to its end, each phase it undoes
marked torn down; the second `q` forces exit, exactly as a second Ctrl-C does. A signal
from outside, a `SIGTERM` from a CI runner or `kill -INT`, is handled as on the plain path
and never closes the view early; should the view close before the run ends for any other
reason, rackctl says so and waits for the run, its rollback included, before it exits.

An interrupted run exits `8` when its unwind finished, and `7` when it did not; the message
starts with `interrupted:` either way. An interrupted `rackctl destroy` exits `8` only when
nothing had been destroyed yet. Once the teardown has begun it leaves components standing,
exits `7`, and re-running the destroy finishes it.

### A teardown that stopped partway

`rackctl destroy` attempts every component even after one fails, then reports them together
and exits `7` (`resources-may-remain`).
The usual causes, in order of likelihood:

- **`DependencyViolation` deleting a security group** — a Karpenter instance still holds it.
  rackctl's sweep terminates instances tagged `karpenter.sh/managed-by=<cluster>` before the
  cluster goes; if it could not enumerate them it says so. Check:
  `aws ec2 describe-instances --filters Name=tag:karpenter.sh/managed-by,Values=<cluster>`
- **`DeleteConflict` on agent-iam** — operator-minted tenant roles survived their finalizer.
  Look under `aws iam list-roles --path-prefix /eks-agent-platform/`.
- **`BucketNotEmpty`** — outside development several buckets refuse a destroy while
  non-empty. Re-run with `--force-buckets`, which applies `force_destroy` into state first
  and then destroys. `force_destroy` has no effect until an apply has landed it. A rollback
  outside development permits it itself, on the components this same run created and no
  others, and prints `rollback: applying force_destroy_buckets=true on …` before it acts.
  Created means the component's state was empty before this run first applied it; one it
  did not apply, or applied over state already there, is named, and the incomplete
  rollback's error names `rackctl destroy --force-buckets`.
- **`could not point kubectl at <cluster>`** — the in-cluster sweeps were skipped. The
  component teardown still ran and terraform state is scoped by state key, so no other
  cluster's Terraform-managed resources were touched. Fix the kubeconfig and re-run the
  destroy.
- **`PVCs did not delete cleanly`** — a pod still mounted the claim. After a verified ArgoCD
  disarm the reap scales every StatefulSet and Deployment outside kube-system that mounts a
  PVC to zero and waits for it before deleting the claims; the line before it names what
  was still running, or why the scale-down was skipped.
- **`in-cluster reap did NOT run — <cluster> exists … but its API does not answer`** — the
  cluster is there and this host cannot reach it: a private endpoint, or an allow-list that
  does not hold this host's egress IP. The line prints both. Re-run from a host the endpoint
  admits so the Platforms, NodeClaims and PVCs are reaped before the cluster goes.

- **`the account-scoped roots … still hold state and were left standing`** — every component
  of this environment came down, and the account's Bedrock invocation-logging root and cost
  pipeline did not, because they are shared by every environment in the account. The exit is
  `7` so an automation does not record an empty account. If other environments still run
  here, that is the intended end state. If this is the last one, re-run with
  `--account-scoped --force-buckets`.
- **`<component> is not in this config but its state holds …`** — the config turned the
  component off after an apply. The destroy tears it down with the rest; nothing to do unless
  its own destroy fails. **`the state of <component>, which this config does not apply, could
  not be read`** is the same case without proof either way: it is not walked, and the run exits
  `7`. Fix the access to the state bucket and re-run.

Both `rackctl destroy` and a rollback end with a **backstop summary**: one line per sweep
that guards something terraform cannot see, saying whether it ran, found nothing, failed or
did not run. `platform destroyed` is printed only when every one of them ran clean; otherwise
the closing line says which did not, and those are what to check before calling the account
empty. A backstop that did not hold exits `7` (`resources-may-remain`), from a destroy whose
components all came down and from a rollback alike. The unprovable-volume report is listed
too, and is informational: it deletes nothing, so it warns and never changes the exit status.
A re-run against a cluster the first run already deleted reports the in-cluster reap as
`found nothing — the cluster no longer exists`, and can finish with `platform destroyed`.

### Resources rackctl will not delete

A sweep deletes only what a resource's own tags prove belongs to this cluster. Anything it
cannot prove is **named and left**, with the reason — most often an EBS volume carrying EBS
CSI provenance and no `kubernetes.io/cluster/*` tag, which might be a sibling environment's.
Deleting it would be a guess. Delete them deliberately:

```sh
aws ec2 delete-volume --volume-id <id>
```

The subdomain delegation is the same. A destroy withdraws the NS record it wrote into the
parent zone only when that record points at the name servers of this account's zone by the
same name. On a re-run where the child zone is already gone, or when the record points
elsewhere, it is left in place, and the line names the servers it points at and the exact
`aws route53 change-resource-record-sets` command that removes it.

### An eks-fleet hub with live spokes

`rackctl destroy` refuses outright, lists them and exits `9` (`refused`): nothing was
destroyed, though kubectl's context now points at the hub. Each spoke is a real EKS cluster
with its own control plane, VPC and NAT gateways, frequently in another account, and this
hub is the only thing that knows they exist. Tear the spokes down first.

With `controlPlane.eksFleet` on, it also refuses, with `9`, when the spokes cannot be
listed: an RBAC denial, a timeout, or a hub this host cannot reach. There is no override.
Make the hub's API reachable from this host and re-run. A `--dry-run` reads the hub for
real and shows the same refusal.

## Where the output went

`rackctl apply --tui` owns the terminal, so subprocess output cannot go to stdout. It is
written to a transcript instead, and the path is printed when the view exits — on success
as well as on failure, because a run that worked is also the one you come back to when a
problem surfaces later.

```
transcript: ~/.rackctl/<org>/logs/apply-1234567890.log
```

The transcript also carries every line the run prints without `--tui`: the rollback
posture, a platform left standing, the rollback and each teardown. Those lines are printed
again after the view exits. The most recent line is rendered under the running phase, so a
long wait shows what it is waiting on. Without `--tui`, output goes to stdout as it happens and no transcript is
written.

## Diagnosing after the fact

`rackctl check` runs the pre-spend checks and, when the ambient kubeconfig points at this
config's cluster, the cluster health checks too. It refuses to assert health against a
different cluster rather than reporting the wrong one's, and a cluster of the same name in
another account or region is a different cluster.

When kubectl reaches no cluster, check asks EKS whether this config's cluster exists. No
cluster yet passes on the pre-spend checks alone. A cluster that exists and that this shell
cannot reach fails with exit `10` and the `aws eks update-kubeconfig` line to fix it. When
EKS cannot answer either, the check reports `not-asserted` and exits `1`.

```sh
rackctl check -c rackctl.yaml
rackctl check -c rackctl.yaml --output json | jq '(.preflight + .platform)[] | select(.status=="fail")'
```

Exit status distinguishes the outcomes — see `docs/exit-codes.md`.

## Environment

Every variable rackctl reads is listed in [`.env.example`](../.env.example). None is
required for a default install: the AWS ones are the standard SDK surface, and `GITHUB_TOKEN`
is needed only when `org.gitops.tenantsRepo` is set.
