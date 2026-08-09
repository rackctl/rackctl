package reap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/exec"
)

// fakeIAM models the IAM API's actual constraints so a test fails against a naive
// implementation, not just a green one:
//
//   - delete-role RETURNS AN ERROR while the role still has attached or inline policies,
//     exactly as the real API does (DeleteConflict). So a reap that deletes before it
//     detaches fails this fake — which is the whole point of asserting the order.
//   - list-roles --path-prefix returns only the roles seeded under that prefix.
type fakeIAM struct {
	attached    map[string][]string // role -> attached managed policy arns
	inline      map[string][]string // role -> inline policy names
	deleted     map[string]bool     // role -> deleted
	detachFails map[string]bool     // role -> detach-role-policy errors (models an API hiccup)
	runs        [][]string          // every mutating command, in order
	// tags is role -> IAM tags. A role with no entry is treated as carrying the tags the
	// operator really stamps, so the tests that predate the ownership check keep testing what
	// they were written to test — the DELETION mechanics — rather than all failing on a
	// concern they were not about. Tests that care about ownership set this explicitly.
	tags map[string]map[string]string
	// tagsFail models a role whose tags cannot be read at all, which must refuse for a
	// different reason than a role that is provably untagged.
	tagsFail map[string]bool
}

func newFakeIAM() *fakeIAM {
	return &fakeIAM{
		attached:    map[string][]string{},
		inline:      map[string][]string{},
		deleted:     map[string]bool{},
		detachFails: map[string]bool{},
		tags:        map[string]map[string]string{},
		tagsFail:    map[string]bool{},
	}
}

// ownedTags is what eks-agent-platform's operator actually puts on a tenant role
// (operators/internal/controller/platform_iam.go:177-190).
func ownedTags() map[string]string {
	return map[string]string{
		"ManagedBy":  "eks-agent-platform",
		"Project":    "eks-agent-platform",
		"Repository": "nanohype/eks-agent-platform",
		"Component":  "tenant-iam",
	}
}

func (f *fakeIAM) Query(ctx context.Context, name string, args ...string) (string, error) {
	if name == "aws" && len(args) > 1 && args[1] == "list-role-tags" {
		role := roleArg(args)
		if f.tagsFail[role] {
			return "", fmt.Errorf("AccessDenied reading tags for %s", role)
		}
		tags, ok := f.tags[role]
		if !ok {
			tags = ownedTags()
		}
		var b strings.Builder
		b.WriteString(`{"Tags":[`)
		first := true
		for k, v := range tags {
			if !first {
				b.WriteString(",")
			}
			first = false
			fmt.Fprintf(&b, `{"Key":%q,"Value":%q}`, k, v)
		}
		b.WriteString("]}")
		return b.String(), nil
	}
	return f.Capture(ctx, name, args...)
}

func (f *fakeIAM) Capture(_ context.Context, name string, args ...string) (string, error) {
	if name != "aws" {
		return "", fmt.Errorf("unexpected capture: %s %v", name, args)
	}
	switch args[1] {
	case "list-roles":
		var names []string
		for r := range f.attached {
			if !f.deleted[r] {
				names = append(names, r)
			}
		}
		// dedup with inline-only roles
		for r := range f.inline {
			if !f.deleted[r] && f.attached[r] == nil {
				names = append(names, r)
			}
		}
		return strings.Join(names, "\t"), nil
	case "list-attached-role-policies":
		return strings.Join(f.attached[roleArg(args)], "\t"), nil
	case "list-role-policies":
		return strings.Join(f.inline[roleArg(args)], "\t"), nil
	}
	return "", fmt.Errorf("unexpected capture: %v", args)
}

func (f *fakeIAM) Run(_ context.Context, name string, args ...string) error {
	f.runs = append(f.runs, append([]string{name}, args...))
	if name != "aws" {
		return fmt.Errorf("unexpected run: %s %v", name, args)
	}
	role := roleArg(args)
	switch args[1] {
	case "detach-role-policy":
		if f.detachFails[role] {
			return fmt.Errorf("Throttling: rate exceeded detaching from %s", role)
		}
		arn := flag(args, "--policy-arn")
		f.attached[role] = remove(f.attached[role], arn)
	case "delete-role-policy":
		f.inline[role] = remove(f.inline[role], flag(args, "--policy-name"))
	case "delete-role":
		if len(f.attached[role]) > 0 || len(f.inline[role]) > 0 {
			return fmt.Errorf("DeleteConflict: role %s still has policies", role)
		}
		f.deleted[role] = true
	}
	return nil
}

func roleArg(args []string) string { return flag(args, "--role-name") }

func flag(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func remove(xs []string, v string) []string {
	out := xs[:0:0]
	for _, x := range xs {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

// The DeleteConflict fix: every operator-minted role is detached and deleted. The fake
// rejects delete-before-detach, so a green run also proves the ordering.
func TestOperatorRoles_ForceDeletesEachRole(t *testing.T) {
	f := newFakeIAM()
	f.attached["dev-ops-tenant"] = []string{"arn:aws:iam::1:policy/tenant-baseline", "arn:aws:iam::1:policy/model-scope"}
	f.inline["dev-ops-tenant"] = []string{"session-inline"}
	f.attached["dev-ops-session"] = []string{"arn:aws:iam::1:policy/attribution"}

	reapOperatorRoles(context.Background(), f, false, &bytes.Buffer{}, Owner{Org: "nanohype", Cluster: "dev"})

	for _, r := range []string{"dev-ops-tenant", "dev-ops-session"} {
		if !f.deleted[r] {
			t.Errorf("%s was not deleted; runs=%v", r, f.runs)
		}
	}
}

// The reap must survive a role it cannot clean without abandoning the others — a backstop
// that stops at the first snag is no backstop. One role's detach fails; the other must
// still be fully reaped.
func TestOperatorRoles_OneFailureDoesNotAbortTheRest(t *testing.T) {
	f := newFakeIAM()
	f.attached["dev-wedged-tenant"] = []string{"arn:aws:iam::1:policy/x"}
	f.detachFails["dev-wedged-tenant"] = true
	f.attached["dev-ok-session"] = []string{"arn:aws:iam::1:policy/y"}

	reapOperatorRoles(context.Background(), f, false, &bytes.Buffer{}, Owner{Org: "nanohype", Cluster: "dev"})

	if f.deleted["dev-wedged-tenant"] {
		t.Errorf("a role whose detach failed must not be deleted (it would DeleteConflict)")
	}
	if !f.deleted["dev-ok-session"] {
		t.Fatalf("a healthy role must still be reaped when another failed; runs=%v", f.runs)
	}
}

func TestOperatorRoles_NoRolesIsClean(t *testing.T) {
	f := newFakeIAM()
	buf := &bytes.Buffer{}
	reapOperatorRoles(context.Background(), f, false, buf, Owner{Org: "nanohype", Cluster: "dev"})
	if len(f.runs) != 0 {
		t.Fatalf("no roles under the prefix => no mutations; got %v", f.runs)
	}
}

func TestOperatorRoles_DryRunTouchesNothing(t *testing.T) {
	f := newFakeIAM()
	f.attached["dev-ops-tenant"] = []string{"arn:aws:iam::1:policy/x"}
	reapOperatorRoles(context.Background(), f, true, &bytes.Buffer{}, Owner{Org: "nanohype", Cluster: "dev"})
	if len(f.runs) != 0 {
		t.Fatalf("dry-run must not mutate; got %v", f.runs)
	}
}

// --- UnstickTerminating ---

// fakeKube models just enough kubectl for the unstick path: a readyz probe, a CRD check,
// a list that returns "<namespace> <name>" lines, and a recorder for patch calls.
type fakeKube struct {
	ready   bool
	crds    map[string]bool   // kind -> installed
	listing map[string]string // kind -> jsonpath output
	patched [][]string        // recorded patch argv (after "kubectl")
}

func (k *fakeKube) Query(ctx context.Context, name string, args ...string) (string, error) {
	return k.Capture(ctx, name, args...)
}

func (k *fakeKube) Capture(_ context.Context, name string, args ...string) (string, error) {
	if name != "kubectl" {
		return "", fmt.Errorf("unexpected: %s", name)
	}
	switch {
	case len(args) >= 2 && args[0] == "get" && args[1] == "--raw":
		if !k.ready {
			return "", fmt.Errorf("connection refused")
		}
		return "ok", nil
	case len(args) >= 3 && args[0] == "get" && args[1] == "crd":
		if k.crds[args[2]] {
			return "yes", nil
		}
		return "", fmt.Errorf("NotFound")
	case len(args) >= 2 && args[0] == "get":
		return k.listing[args[1]], nil
	}
	return "", fmt.Errorf("unexpected capture: %v", args)
}

func (k *fakeKube) Run(_ context.Context, name string, args ...string) error {
	if name == "kubectl" && len(args) > 0 && contains(args, "patch") {
		k.patched = append(k.patched, args)
	}
	return nil
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// A namespaced Platform stuck Terminating is patched WITH -n; a cluster-scoped Tenant is
// patched WITHOUT one. Getting that wrong makes the patch target the wrong object (or none).
func TestUnstickTerminating_NamespacedAndClusterScoped(t *testing.T) {
	k := &fakeKube{
		ready: true,
		crds: map[string]bool{
			"platforms.platform.nanohype.dev": true,
			"tenants.platform.nanohype.dev":   true,
		},
		listing: map[string]string{
			"platforms.platform.nanohype.dev": "acme-team ops\n", // namespace + name
			"tenants.platform.nanohype.dev":   " acme\n",         // cluster-scoped: leading space, no ns
		},
	}
	unstickTerminating(context.Background(), k, false, &bytes.Buffer{})

	if len(k.patched) != 2 {
		t.Fatalf("both stuck CRs must be unstuck; got %d patches: %v", len(k.patched), k.patched)
	}
	var sawNamespaced, sawClusterScoped bool
	for _, p := range k.patched {
		if contains(p, "-n") && contains(p, "acme-team") && contains(p, "ops") {
			sawNamespaced = true
		}
		if !contains(p, "-n") && contains(p, "acme") {
			sawClusterScoped = true
		}
	}
	if !sawNamespaced {
		t.Errorf("namespaced Platform must be patched with -n <namespace>; got %v", k.patched)
	}
	if !sawClusterScoped {
		t.Errorf("cluster-scoped Tenant must be patched without -n; got %v", k.patched)
	}
}

func TestUnstickTerminating_NothingStuck(t *testing.T) {
	k := &fakeKube{
		ready:   true,
		crds:    map[string]bool{"platforms.platform.nanohype.dev": true},
		listing: map[string]string{"platforms.platform.nanohype.dev": ""},
	}
	unstickTerminating(context.Background(), k, false, &bytes.Buffer{})
	if len(k.patched) != 0 {
		t.Fatalf("no Terminating CRs => no patches; got %v", k.patched)
	}
}

func TestUnstickTerminating_NoClusterIsClean(t *testing.T) {
	k := &fakeKube{ready: false}
	unstickTerminating(context.Background(), k, false, &bytes.Buffer{})
	if len(k.patched) != 0 {
		t.Fatalf("unreachable cluster => nothing to do; got %v", k.patched)
	}
}

// The sweep is account-wide by construction — IAM is global, and the operator's tenant path
// carries neither an environment nor a cluster segment — so the ONLY thing keeping it inside
// the cluster being torn down is the name filter.
//
// An account hosting development and staging has both clusters' tenant roles under
// /eks-agent-platform/tenants/. Without the filter, `rackctl destroy` against staging
// force-detaches and deletes development's live tenant roles: the agent pods there keep
// running with cached credentials until they rotate, then start failing AssumeRole with
// nothing in the cluster to explain why, because the thing that deleted their role was a
// teardown of a different cluster in a different environment.
func TestOperatorRoles_LeavesAnotherClustersRolesAlone(t *testing.T) {
	f := newFakeIAM()
	f.attached["staging-ops-tenant"] = []string{"arn:aws:iam::1:policy/x"}
	f.attached["development-ops-tenant"] = []string{"arn:aws:iam::1:policy/x"}

	reapOperatorRoles(context.Background(), f, false, &bytes.Buffer{}, Owner{Org: "nanohype", Cluster: "staging-ops"})

	if !f.deleted["staging-ops-tenant"] {
		t.Errorf("the torn-down cluster's own role must still be reaped; runs=%v", f.runs)
	}
	if f.deleted["development-ops-tenant"] {
		t.Fatalf("destroying staging deleted a DEVELOPMENT tenant role — the sweep is scoped by "+
			"name because the IAM path is shared across every cluster in the account; runs=%v", f.runs)
	}
	for _, r := range f.runs {
		if flag(r, "--role-name") == "development-ops-tenant" {
			t.Fatalf("no mutation may touch another cluster's role, not even a detach; got %v", r)
		}
	}
}

// A cluster name of "" would make `HasPrefix(name, "-")`… match nothing by luck, but
// `HasPrefix(name, cluster+"-")` with an empty cluster is one edit away from matching
// everything, and a teardown is the wrong place to rely on luck. There is no cluster whose
// roles could be named for an empty name, so the correct behaviour is to do nothing at all —
// not to enumerate and filter.
func TestOperatorRoles_BlankClusterReapsNothing(t *testing.T) {
	f := &recordingIAM{fakeIAM: newFakeIAM()}
	f.attached["development-ops-tenant"] = []string{"arn:aws:iam::1:policy/x"}

	reapOperatorRoles(context.Background(), f, false, &bytes.Buffer{}, Owner{Org: "nanohype", Cluster: ""})

	if len(f.runs) != 0 {
		t.Fatalf("a blank cluster name must reap nothing, not everything; got %v", f.runs)
	}
	// Asserting no MUTATION is not enough, and this is the whole reason the test exists.
	// With the guard removed, HasPrefix(name, "-") matches nothing by accident, so the sweep
	// enumerates, filters to zero and mutates nothing — a green test over a missing guard.
	// The documented contract is "do nothing at all, not enumerate and filter", so the
	// enumeration itself is what has to be absent.
	if f.listArgs != nil {
		t.Fatalf("a blank cluster name must not even enumerate: the empty prefix matching nothing "+
			"is an accident of the name shape, not a guard, and one edit to the filter turns it "+
			"into an account-wide sweep.\ngot: %v", f.listArgs)
	}
}

// agent-iam's own terraform owns one ROLE at the SHALLOWER path /eks-agent-platform/ —
// <cluster>-agent-platform-operator — and destroys it in the ordinary way moments later. It is
// named for the cluster, so the name filter does not exclude it; the PATH is what does. (The
// tenant boundary and baseline sit at that path too, but are aws_iam_policy, so list-roles
// never returns them.)
//
// This asserts the path, because the two filters protect against different things and passing
// on the name alone would leave terraform planning a delete for a role rackctl already
// force-deleted. The fake serves list-roles for one prefix only, so seeding a role and
// asserting the query is the way to pin it.
func TestOperatorRoles_QueriesOnlyTheTenantPath(t *testing.T) {
	f := &recordingIAM{fakeIAM: newFakeIAM()}
	f.attached["development-ops-tenant"] = []string{"arn:aws:iam::1:policy/x"}

	reapOperatorRoles(context.Background(), f, false, &bytes.Buffer{}, Owner{Org: "nanohype", Cluster: "development-ops"})

	if got := flag(f.listArgs, "--path-prefix"); got != "/eks-agent-platform/tenants/" {
		t.Fatalf("list-roles asked for --path-prefix %q; the operator mints tenant and session "+
			"roles under /eks-agent-platform/tenants/, while the shallower /eks-agent-platform/ "+
			"also holds <cluster>-agent-platform-operator, which agent-iam's own terraform owns "+
			"and is about to destroy itself", got)
	}
}

// recordingIAM captures the list-roles argv so a test can assert the path prefix, which is
// otherwise invisible: the fake answers every prefix identically.
type recordingIAM struct {
	*fakeIAM
	listArgs []string
}

func (r *recordingIAM) Capture(ctx context.Context, name string, args ...string) (string, error) {
	if len(args) > 1 && args[1] == "list-roles" {
		r.listArgs = append([]string{name}, args...)
	}
	return r.fakeIAM.Capture(ctx, name, args...)
}

// Query has to be overridden too, and the reason is worth stating because it is exactly what
// this test caught. Enumeration moved from Capture to Query so a dry-run can demonstrate its
// selection rather than describe it. Go embedding has no virtual dispatch, so fakeIAM.Query
// calls fakeIAM.Capture — never this recorder — and the path-prefix assertion would have gone
// quietly blind while still passing on a hardcoded default. It failed instead, which is the
// behaviour a test asserting argv is supposed to have.
func (r *recordingIAM) Query(ctx context.Context, name string, args ...string) (string, error) {
	if len(args) > 1 && args[1] == "list-roles" {
		r.listArgs = append([]string{name}, args...)
	}
	return r.fakeIAM.Query(ctx, name, args...)
}

// PointAt is the safety argument for the two ambient sweeps, so it has to fail closed.
//
// reap.All and reap.UnstickTerminating act on whatever context kubectl resolves, guarded only
// by a /readyz probe that tests liveness and never identity. `rackctl destroy` never touched
// the kubeconfig at all, so a teardown of staging from a shell pointed at a healthy
// development cluster deleted every Platform, Tenant, NodeClaim and PVC in development and
// then stripped their finalizers — with the IAM roles those CRs guarded deliberately left
// alive, which orphans exactly the AWS state this package exists never to orphan.
func TestPointAt_NamesTheClusterBeingDestroyed(t *testing.T) {
	f := newFakeIAM()
	if err := pointAt(context.Background(), f, "staging-platform"); err != nil {
		t.Fatalf("pointAt: %v", err)
	}
	var found bool
	for _, r := range f.runs {
		if len(r) >= 5 && r[1] == "eks" && r[2] == "update-kubeconfig" && flag(r, "--name") == "staging-platform" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the kubeconfig must be repointed at the cluster being torn down before anything "+
			"sweeps ambient Kubernetes state; got %v", f.runs)
	}
}

// A blank cluster name must be an error, not a no-op that reports success — the caller uses
// the return value to decide whether the sweeps may run at all.
func TestPointAt_RefusesABlankCluster(t *testing.T) {
	f := newFakeIAM()
	if err := pointAt(context.Background(), f, ""); err == nil {
		t.Fatal("a blank cluster name must fail, so the caller skips the ambient sweeps rather " +
			"than running them against whatever the operator was last pointed at")
	}
	if len(f.runs) != 0 {
		t.Fatalf("nothing should be executed for a blank cluster; got %v", f.runs)
	}
}

// A hub with vended spokes must be discoverable before anything is destroyed.
//
// A Cluster CR composes a whole landing-zone stack through provider-opentofu — a real EKS
// control plane, VPC and NAT gateways, frequently in another AWS account. The hub is the
// only place they are tracked, so a teardown that does not see them strands them past the
// point of recovery.
func TestFleetSpokes_NamesEveryVendedCluster(t *testing.T) {
	got := fleetSpokes(context.Background(), &spokeExecer{out: "team-a/orders  team-b/analytics "})
	want := []string{"team-a/orders", "team-b/analytics"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// No CRD installed (fleet was never enabled) and no reachable cluster both mean the same
// thing: there is no hub, so there is nothing to strand. Neither may block a teardown.
func TestFleetSpokes_EmptyWhenThereIsNoHub(t *testing.T) {
	noCRD := &spokeExecer{err: errors.New(`error: the server doesn't have a resource type "clusters"`)}
	if got := fleetSpokes(context.Background(), noCRD); len(got) != 0 {
		t.Fatalf("got %v, want none — an absent CRD must not block a teardown", got)
	}
}

// spokeExecer answers the one Capture FleetSpokes makes.
type spokeExecer struct {
	out string
	err error
}

func (s *spokeExecer) Run(context.Context, string, ...string) error { return nil }
func (s *spokeExecer) Capture(context.Context, string, ...string) (string, error) {
	return s.out, s.err
}
func (s *spokeExecer) Query(context.Context, string, ...string) (string, error) {
	return s.out, s.err
}

// --- ownership: the sweep must prove a role is ours before force-deleting it ---
//
// The path and the name are structural guesses about a resource. The tags are the resource's
// own statement about itself. In a dedicated account the difference is academic; in the
// account that has been used for anything else it is not, because the org tagging standard
// pins one Project value for every deployment that follows it — including whichever one owns
// the account's CloudTrail, its CUR, or its mail.

func TestOperatorRoles_RefusesARoleThatCannotBeProvedOurs(t *testing.T) {
	cases := []struct {
		name string
		tags map[string]string
		why  string
	}{
		{
			name: "untagged",
			tags: map[string]string{},
			why:  "untagged means unknown, and unknown must be treated as someone else's",
		},
		{
			name: "another estate under the same Project value",
			tags: map[string]string{
				"Project":    "landing-zone",
				"ManagedBy":  "opentofu",
				"Repository": "stxkxs/landing-zone",
			},
			why: "Project=landing-zone and ManagedBy=opentofu are shared across estates in this " +
				"account; only Repository discriminates, and this one is not ours",
		},
		{
			name: "the near-miss: right Project, no Repository",
			tags: map[string]string{"Project": "eks-agent-platform", "Environment": "development"},
			why:  "a matching Project is exactly the trap — it proves nothing on its own",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeIAM()
			f.attached["dev-ops-tenant"] = []string{"arn:aws:iam::1:policy/x"}
			f.tags["dev-ops-tenant"] = tc.tags

			buf := &bytes.Buffer{}
			reapOperatorRoles(context.Background(), f, false, buf, Owner{Org: "nanohype", Cluster: "dev"})

			if f.deleted["dev-ops-tenant"] {
				t.Fatalf("force-deleted a role it could not prove it owned: %s", tc.why)
			}
			if len(f.runs) != 0 {
				t.Fatalf("a refused role must not be mutated at all (no detach, no delete); got %v", f.runs)
			}
			if !strings.Contains(buf.String(), "REFUSING") {
				t.Fatalf("a refusal must be loud — a silent skip reads as a clean teardown and the "+
					"operator only finds out when agent-iam fails on DeleteConflict.\n%s", buf.String())
			}
		})
	}
}

// A role whose tags cannot be READ is a different failure from one that is provably untagged,
// and collapsing them would be the wrong kind of safe: it would look like a decision when it
// was an outage.
func TestOperatorRoles_RefusesWhenTagsCannotBeRead(t *testing.T) {
	f := newFakeIAM()
	f.attached["dev-ops-tenant"] = []string{"arn:aws:iam::1:policy/x"}
	f.tagsFail["dev-ops-tenant"] = true

	buf := &bytes.Buffer{}
	reapOperatorRoles(context.Background(), f, false, buf, Owner{Org: "nanohype", Cluster: "dev"})

	if f.deleted["dev-ops-tenant"] {
		t.Fatal("a role whose ownership could not be determined must not be force-deleted")
	}
	if !strings.Contains(buf.String(), "could not read its tags") {
		t.Fatalf("the reason must distinguish 'unreadable' from 'not ours'.\n%s", buf.String())
	}
}

// The other half, and the one that keeps the guard honest: a role that IS ours still gets
// reaped. A predicate that refuses everything would pass every test above and break every
// teardown.
func TestOperatorRoles_StillReapsAProvablyOwnedRole(t *testing.T) {
	f := newFakeIAM()
	f.attached["dev-ops-tenant"] = []string{"arn:aws:iam::1:policy/x"}
	f.tags["dev-ops-tenant"] = map[string]string{"Repository": "nanohype/eks-agent-platform"}
	f.attached["dev-ops-session"] = []string{"arn:aws:iam::1:policy/y"}
	f.tags["dev-ops-session"] = map[string]string{"ManagedBy": "eks-agent-platform"}

	reapOperatorRoles(context.Background(), f, false, &bytes.Buffer{}, Owner{Org: "nanohype", Cluster: "dev"})

	for _, r := range []string{"dev-ops-tenant", "dev-ops-session"} {
		if !f.deleted[r] {
			t.Errorf("%s is provably ours and must still be reaped; runs=%v", r, f.runs)
		}
	}
}

// The predicate belongs to the RUN's org, not to a hardcoded list of nanohype's repo names.
// An operator installing from their own fork must not have every sweep refuse.
func TestOwner_ProvesIsScopedToTheRunsOrg(t *testing.T) {
	own := Owner{Org: "acme", Cluster: "dev-ops"}
	if ok, _ := own.Proves(map[string]string{"Repository": "acme/landing-zone"}); !ok {
		t.Fatal("a resource tagged with the run's own org must be provable")
	}
	if ok, _ := own.Proves(map[string]string{"Repository": "nanohype/landing-zone"}); ok {
		t.Fatal("another org's resource must not be provable just because it is nanohype's")
	}
}

// A dry-run must ENUMERATE, not just describe. The whole point of the negative test is that
// `rackctl destroy` without --apply can be pointed at a live account and shown to select
// nothing pre-existing; a dry-run that queries nothing cannot show anything.
func TestOperatorRoles_DryRunEnumeratesSoItCanBeNegativeTested(t *testing.T) {
	f := &recordingIAM{fakeIAM: newFakeIAM()}
	f.attached["dev-ops-tenant"] = []string{"arn:aws:iam::1:policy/x"}

	buf := &bytes.Buffer{}
	reapOperatorRoles(context.Background(), f, true, buf, Owner{Org: "nanohype", Cluster: "dev"})

	if f.listArgs == nil {
		t.Fatal("a dry-run must actually enumerate: describing the filter back to the operator is " +
			"not evidence about what the filter selects, and 'selects zero' is the claim the " +
			"whole sweep rests on")
	}
	if len(f.runs) != 0 {
		t.Fatalf("enumerating in dry-run must not become mutating in dry-run; got %v", f.runs)
	}
	if !strings.Contains(buf.String(), "dev-ops-tenant") {
		t.Fatalf("the dry-run must name what it would delete.\n%s", buf.String())
	}
}

// The reap must disarm ArgoCD before deleting anything a controller owns.
//
// Every catalog Application carries automated.selfHeal, and a Platform CR is
// catalog-managed — so deleting one is drift, and ArgoCD corrects drift. Observed on a
// live teardown: the reap deleted Platform/ops, its finalizer removed the tenant's IAM
// roles, and ArgoCD recreated the Platform seconds later, which made the operator mint
// the roles again. agent-iam then fails on DeleteConflict several components later,
// naming a managed policy rather than the race that repopulated it.
//
// The assertion is on ORDER, not merely on presence: patching after the deletes would be
// a no-op with a reassuring log line.

// fakeKubectl puts a kubectl on PATH that records every invocation and answers the two
// probes All() gates on: /readyz (is there a cluster) and `get crd` (is the CRD
// installed). Everything else succeeds, because this asserts WHICH commands run and in
// what order, not what they return.
func fakeKubectl(t *testing.T) func() []string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
exit 0
`, logPath)
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake kubectl: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() []string {
		b, err := os.ReadFile(logPath)
		if err != nil {
			return nil
		}
		var out []string
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if l != "" {
				out = append(out, l)
			}
		}
		return out
	}
}

func TestAll_DisarmsArgoCDBeforeReapingAnything(t *testing.T) {
	calls := fakeKubectl(t)

	All(context.Background(), exec.New(io.Discard), io.Discard)

	got := calls()
	patchAt, deleteAt := -1, -1
	for i, c := range got {
		if patchAt < 0 && strings.Contains(c, "patch applications") && strings.Contains(c, "syncPolicy") {
			patchAt = i
		}
		if deleteAt < 0 && strings.Contains(c, "delete platforms.platform.nanohype.dev") {
			deleteAt = i
		}
	}
	if patchAt < 0 {
		t.Fatalf("syncPolicy was never cleared — selfHeal recreates every CR this reap deletes.\ncalls: %#v", got)
	}
	if deleteAt < 0 {
		t.Fatalf("the Platform reap did not run at all.\ncalls: %#v", got)
	}
	if patchAt > deleteAt {
		t.Fatalf("ArgoCD was disarmed AFTER the delete (%d > %d) — by then it has already put the "+
			"Platform back.\ncalls: %#v", patchAt, deleteAt, got)
	}
}
