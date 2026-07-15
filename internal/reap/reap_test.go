package reap

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
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
}

func newFakeIAM() *fakeIAM {
	return &fakeIAM{
		attached:    map[string][]string{},
		inline:      map[string][]string{},
		deleted:     map[string]bool{},
		detachFails: map[string]bool{},
	}
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

	reapOperatorRoles(context.Background(), f, false, &bytes.Buffer{})

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

	reapOperatorRoles(context.Background(), f, false, &bytes.Buffer{})

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
	reapOperatorRoles(context.Background(), f, false, buf)
	if len(f.runs) != 0 {
		t.Fatalf("no roles under the prefix => no mutations; got %v", f.runs)
	}
}

func TestOperatorRoles_DryRunTouchesNothing(t *testing.T) {
	f := newFakeIAM()
	f.attached["dev-ops-tenant"] = []string{"arn:aws:iam::1:policy/x"}
	reapOperatorRoles(context.Background(), f, true, &bytes.Buffer{})
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
