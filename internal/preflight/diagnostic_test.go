package preflight

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A verdict that reports a problem must carry the reason the tool gave.
//
// Every check here ends in a Result an operator reads and acts on. When the underlying
// call fails, the tool's own stderr — AccessDenied, a throttle, the wrong region — is in
// the error, and a verdict that says "could not list hosted zones" and discards it has
// destroyed the one artifact explaining itself at the exact moment it became useful. It
// then costs the operator a search that rackctl already did.
//
// The test is whether the verdict carries A DIAGNOSIS, not whether it names `err`
// specifically. Several checks fail on a PARSE rather than on a call, and print the
// offending value — "unreadable quota value: <out>" diagnoses precisely, and appending the
// strconv error would add nothing. The defect is a verdict whose message is entirely
// CONSTANT: it reports that something went wrong and carries not one byte about what.
//
// Asserted over the AST rather than by grep, because the shape is structural: a warn/fail
// return inside an `if err != nil` body, every argument of which is a literal.
//
// An `ok` or `skip` is exempt. Both are deliberate mappings of an error onto "nothing to
// check here" — CheckSessionLifetime reads an unreadable expiry as "no expiring
// credentials", and appending an exit status to a healthy verdict is noise rather than
// diagnosis.
func TestChecks_AVerdictOnAFailureCarriesItsReason(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}

	var scanned, verdicts int
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		scanned++

		ast.Inspect(f, func(n ast.Node) bool {
			ifStmt, ok := n.(*ast.IfStmt)
			if !ok || !comparesErrToNil(ifStmt.Cond) {
				return true
			}
			ast.Inspect(ifStmt.Body, func(m ast.Node) bool {
				call, ok := m.(*ast.CallExpr)
				if !ok {
					return true
				}
				fn, ok := call.Fun.(*ast.Ident)
				if !ok || (fn.Name != "warn" && fn.Name != "fail") {
					return true
				}
				verdicts++
				if !carriesDiagnosis(call) {
					pos := fset.Position(call.Pos())
					t.Errorf("%s:%d: %s(...) reports a failure with an entirely constant "+
						"message — it says something went wrong and not one byte about what, "+
						"so the operator is sent to find out what rackctl already knew",
						pos.Filename, pos.Line, fn.Name)
				}
				return true
			})
			return true
		})
	}

	// The denominator. A pass over zero verdicts and a pass over all of them read
	// identically, and the first means the walk stopped finding them.
	t.Logf("%d file(s) scanned, %d failure verdict(s) inside an error branch checked",
		scanned, verdicts)
	if scanned == 0 || verdicts == 0 {
		t.Fatal("this test examined nothing — a walk that matches nothing reports success " +
			"over zero targets")
	}
}

// comparesErrToNil reports whether a condition is `err != nil`.
func comparesErrToNil(cond ast.Expr) bool {
	bin, ok := cond.(*ast.BinaryExpr)
	if !ok || bin.Op != token.NEQ {
		return false
	}
	x, okX := bin.X.(*ast.Ident)
	y, okY := bin.Y.(*ast.Ident)
	return okX && okY && x.Name == "err" && y.Name == "nil"
}

// carriesDiagnosis reports whether a verdict interpolates anything at all.
//
// `name` is the check's own const and is not a diagnosis, so it is discounted: a call of
// warn(name, "could not list secrets") is two constants and reports nothing.
func carriesDiagnosis(call *ast.CallExpr) bool {
	found := false
	for _, arg := range call.Args {
		ast.Inspect(arg, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.Ident:
				if v.Name != "name" {
					found = true
				}
			case *ast.CallExpr, *ast.SelectorExpr, *ast.IndexExpr:
				found = true
			}
			return !found
		})
	}
	return found
}

// The detector must be able to produce a POSITIVE, or a clean sweep says nothing.
func TestVerdictDetector_FiresOnASwallowedError(t *testing.T) {
	const src = `package p
func f() Result {
	out, err := g()
	if err != nil {
		return warn(name, "could not read it")
	}
	if err != nil {
		return warn(name, "could not read it: "+err.Error())
	}
	return ok(name, out)
}`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	var swallowed, carried int
	ast.Inspect(f, func(n ast.Node) bool {
		ifStmt, ok := n.(*ast.IfStmt)
		if !ok || !comparesErrToNil(ifStmt.Cond) {
			return true
		}
		ast.Inspect(ifStmt.Body, func(m ast.Node) bool {
			call, ok := m.(*ast.CallExpr)
			if !ok {
				return true
			}
			if fn, ok := call.Fun.(*ast.Ident); ok && fn.Name == "warn" {
				if carriesDiagnosis(call) {
					carried++
				} else {
					swallowed++
				}
			}
			return true
		})
		return true
	})

	if swallowed != 1 {
		t.Errorf("the detector found %d swallowed verdicts in a fixture with exactly 1 — a "+
			"detector that cannot produce a positive makes the sweep above meaningless", swallowed)
	}
	if carried != 1 {
		t.Errorf("the detector found %d carrying verdicts in a fixture with exactly 1 — one "+
			"that flags everything is as useless as one that flags nothing", carried)
	}
}
