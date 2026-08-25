# rackctl runbook

What to do when an install, a health check or a teardown goes wrong. `rackctl apply`
provisions a VPC, an EKS control plane and a full AWS substrate, so a failure partway
leaves real, billable resources behind — the question is always which ones, and whether
the tool already removed them.

## Before you run anything destructive

`rackctl apply` and `rackctl destroy` print a banner first:

```
rackctl destroy — acme · 477165397189 · acme-production · us-east-1 · production
                  ↑ org   ↑ account      ↑ profile         ↑ region   ↑ environment
```

Confirm the **account** and the **profile** in that line before you proceed. The region is
shared by every account you have and the org name says nothing about which credentials are
in scope, so those two are the only fields that identify the cloud about to change.

`rackctl destroy` then asks you to type the cluster name. That is deliberately worse
ergonomics than a y/n: the failure this guards is a reflex failure, and the name is the one
thing you cannot type correctly while thinking about a different cluster.

## Did the rollback run?

On a failed phase, `rackctl apply` tears down the phases it completed, in reverse — but
**not always**, and the distinction decides your next command.

| What you see | What happened | What to do |
|---|---|---|
| `failure detected — rolling back provisioned resources` | The rollback ran. Cloud created by this run is being destroyed. | Wait for it to finish. If it reports `the rollback did not complete`, see *A teardown that stopped partway*. |
| `existing platform detected — a failure will NOT roll it back` | The platform was already up before this run. Nothing was destroyed. | Fix the failure and re-run `rackctl apply`. It is re-runnable. |
| `could not determine whether a platform already exists — rollback is DISABLED` | The EKS list call failed, so rackctl could not tell a fresh install from a re-apply, and fails closed. | Fix credentials or connectivity, then re-run. Nothing was destroyed. |
| `this is an optional phase and the platform underneath it is provisioned` | Phase 7, 8 or 9 failed. The core platform is up and the run continued. | `rackctl check` to see what is wrong. The platform is usable. |
| `the core platform is up; optional phases failed` | Same, reported at the end. The exit status is still non-zero. | As above. |

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
- **`bucket names`** — an S3 name this run would create is already taken. S3 names are
  globally unique across every AWS account, so this is unrecoverable by retry: rename the
  cluster.
- **`vcpu quota`** — a fresh account caps at ~32 vCPU. rackctl files the increase itself
  unless `quotas.autoRequest` is false; AWS takes minutes to hours to grant it.
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
consecutive samples at an unchanged count. On expiry it names what is unhealthy and returns
`NoRollbackError`, so the cloud stays standing.

Some workloads converge slowly by design: opencost crashloops until its metrics reach AMP,
which takes minutes of scraping. A wait that expires with almost everything Healthy is not a
failed install.

```sh
kubectl -n argocd get applications
rackctl check -c rackctl.yaml     # names what is wrong, per check
```

### An interrupt

The first Ctrl-C cancels the run's context, which terminates the in-flight terragrunt,
kubectl or helm rather than orphaning it, and lets the rollback run. **The second Ctrl-C
kills the process outright** — including a rollback in progress, which will leave resources
behind. Use it only if the unwind itself has hung.

### A teardown that stopped partway

`rackctl destroy` attempts every component even after one fails, then reports them together.
The usual causes, in order of likelihood:

- **`DependencyViolation` deleting a security group** — a Karpenter instance still holds it.
  rackctl's sweep terminates instances tagged `karpenter.sh/managed-by=<cluster>` before the
  cluster goes; if it could not enumerate them it says so. Check:
  `aws ec2 describe-instances --filters Name=tag:karpenter.sh/managed-by,Values=<cluster>`
- **`DeleteConflict` on agent-iam** — operator-minted tenant roles survived their finalizer.
  Look under `aws iam list-roles --path-prefix /eks-agent-platform/`.
- **`BucketNotEmpty`** — outside development several buckets refuse a destroy while
  non-empty. Re-run with `--force-buckets`, which applies `force_destroy` into state first
  and then destroys. `force_destroy` has no effect until an apply has landed it.
- **`could not point kubectl at <cluster>`** — the in-cluster sweeps were skipped. The
  component teardown still ran and terraform state is scoped by state key, so nothing
  belonging to another cluster was touched. Fix the kubeconfig and re-run the destroy.

### Resources rackctl will not delete

A sweep deletes only what a resource's own tags prove belongs to this cluster. Anything it
cannot prove is **named and left**, with the reason — most often an EBS volume carrying EBS
CSI provenance and no `kubernetes.io/cluster/*` tag, which might be a sibling environment's.
Deleting it would be a guess. Delete them deliberately:

```sh
aws ec2 delete-volume --volume-id <id>
```

### An eks-fleet hub with live spokes

`rackctl destroy` refuses outright and lists them. Each spoke is a real EKS cluster with its
own control plane, VPC and NAT gateways, frequently in another account, and this hub is the
only thing that knows they exist. Tear the spokes down first.

## Diagnosing after the fact

`rackctl check` runs the pre-spend checks and, when the ambient kubeconfig points at this
config's cluster, the cluster health checks too. It refuses to assert health against a
different cluster rather than reporting the wrong one's.

```sh
rackctl check -c rackctl.yaml
rackctl check -c rackctl.yaml --output json | jq '.results[] | select(.status=="fail")'
```

Exit status distinguishes the outcomes — see `docs/exit-codes.md`.

## Environment

Every variable rackctl reads is listed in [`.env.example`](../.env.example). None is
required for a default install: the AWS ones are the standard SDK surface, and `GITHUB_TOKEN`
is needed only when `org.gitops.tenantsRepo` is set.
