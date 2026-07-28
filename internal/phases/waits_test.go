package phases

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Every `kubectl wait --for=condition=X` in this package must name a condition something
// actually writes.
//
// This is a source-level test rather than a behavioural one on purpose: the bug it pins was
// written TWICE, in one file, and the second instance survived a pass that found and fixed
// the first. Both times the mistake was the same — reading a status field that happens to be
// spelled like a condition (`Healthy`, `Ready`) and waiting on it as though it were one.
// `kubectl wait` cannot tell you it is wrong: an unsatisfiable condition is indistinguishable
// from a slow one until the timeout expires, so both bugs presented as a 15- or 30-minute
// hang on a platform that was already up.
//
//   - ArgoCD Application: NO Healthy condition. `ApplicationConditionType` is error/warning
//     types only (InvalidSpecError, ComparisonError, SyncError, SharedResourceWarning,
//     OrphanedResourceWarning, …). Health is `.status.health.status`.
//   - Platform (platform.nanohype.dev): NO Ready condition. The operator sets
//     `status.phase = "Ready"`; the conditions it writes are Suspended, ModelAccessScoped,
//     NamespaceReady and VClusterReady.
//
// The allow-list below is every condition type Kubernetes itself writes and that this
// package legitimately waits on. Adding to it should require knowing which controller writes
// the condition — that is the question the two bugs failed to ask.
func TestKubectlWaits_OnlyNameConditionsSomethingWrites(t *testing.T) {
	// Written by the apiextensions controller on a CustomResourceDefinition, and by the
	// deployment controller on a Deployment. Both are built-in and genuinely published.
	allowed := map[string]string{
		"Established": "apiextensions.k8s.io writes it on a CustomResourceDefinition",
		"Available":   "the deployment controller writes it on a Deployment",
	}

	// Status fields commonly mistaken for conditions. Naming them explicitly makes the
	// failure message teach the distinction instead of just reporting a mismatch.
	notConditions := map[string]string{
		"Healthy": "ArgoCD publishes health at .status.health.status, not as a condition — " +
			"use --for=jsonpath={.status.health.status}=Healthy",
		"Ready": "a Platform reports readiness as .status.phase, not as a condition — " +
			"use --for=jsonpath={.status.phase}=Ready",
	}

	// Scan string LITERALS via the AST, not raw source. The comments in this package
	// deliberately quote the broken forms while explaining why they are broken — that prose
	// is the fix, and a text scan would flag it and push someone to delete the explanation
	// to make the test pass.
	re := regexp.MustCompile(`--for=condition=([A-Za-z]+)`)

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	var scanned int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++
		// Walk CallExprs, not individual string lits. A wait is almost always split across
		// several args (`"wait", "--for=condition=Healthy", "provider/…"`), and the resource
		// kind is what decides whether Healthy is the ArgoCD trap or a real Crossplane
		// condition. Joining every string arg in the call is what makes that distinction.
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var parts []string
			var pos token.Pos
			for _, a := range call.Args {
				lit, ok := a.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				val, err := strconv.Unquote(lit.Value)
				if err != nil {
					continue
				}
				if pos == 0 {
					pos = lit.Pos()
				}
				parts = append(parts, val)
			}
			if len(parts) == 0 {
				return true
			}
			joined := strings.Join(parts, " ")
			for _, m := range re.FindAllStringSubmatch(joined, -1) {
				cond := m[1]
				if _, ok := allowed[cond]; ok {
					continue
				}
				// Crossplane's package controller writes Healthy on Provider packages.
				// Accept only when the same call names a provider resource, so an
				// Application wait cannot smuggle Healthy back in.
				if cond == "Healthy" && (strings.Contains(joined, "provider/") ||
					strings.Contains(joined, "provider.pkg.crossplane.io")) {
					continue
				}
				p := fset.Position(pos)
				if why, bad := notConditions[cond]; bad {
					t.Errorf("%s:%d waits on --for=condition=%s, which nothing writes: %s",
						name, p.Line, cond, why)
					continue
				}
				t.Errorf("%s:%d waits on --for=condition=%s, which is not in this test's allow-list. "+
					"Before adding it, name the controller that writes that condition — if none does, "+
					"the wait can never be satisfied and will hang until its timeout.", name, p.Line, cond)
			}
			return true
		})
	}
	if scanned == 0 {
		t.Fatal("scanned no source files — the test is not looking at anything")
	}
}
