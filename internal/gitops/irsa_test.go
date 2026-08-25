package gitops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSubstituteAccountID(t *testing.T) {
	in := "roleArn: arn:aws:iam::000000000000:role/cert-manager\nbucket: assets-000000000000"
	out, n := SubstituteAccountID(in, "123456789012")
	if n != 2 {
		t.Fatalf("replacements = %d, want 2", n)
	}
	if strings.Contains(out, Placeholder) {
		t.Fatalf("placeholder still present: %q", out)
	}
	if strings.Count(out, "123456789012") != 2 {
		t.Fatalf("account id not substituted twice: %q", out)
	}

	if _, n := SubstituteAccountID("no placeholder here", "123456789012"); n != 0 {
		t.Fatalf("replacements = %d, want 0", n)
	}
}

func TestWriteBack(t *testing.T) {
	dir := t.TempDir()
	addon := filepath.Join(dir, "addons", "cert-manager")
	if err := os.MkdirAll(addon, 0o755); err != nil {
		t.Fatal(err)
	}
	dev := filepath.Join(addon, "values-development.yaml")
	if err := os.WriteFile(dev, []byte("arn: arn:aws:iam::000000000000:role/cm"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A different env's file must NOT be touched.
	prod := filepath.Join(addon, "values-production.yaml")
	if err := os.WriteFile(prod, []byte("arn: arn:aws:iam::000000000000:role/cm"), 0o644); err != nil {
		t.Fatal(err)
	}

	n, changed, err := WriteBack(dir, "development", "123456789012")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("replacements = %d, want 1", n)
	}
	if len(changed) != 1 || filepath.Base(changed[0]) != "values-development.yaml" {
		t.Fatalf("changed = %v, want [values-development.yaml]", changed)
	}

	got, _ := os.ReadFile(dev)
	if strings.Contains(string(got), Placeholder) {
		t.Fatalf("dev file still has placeholder: %s", got)
	}
	untouched, _ := os.ReadFile(prod)
	if !strings.Contains(string(untouched), Placeholder) {
		t.Fatalf("prod file was modified but should be untouched: %s", untouched)
	}
}

// This is a WRITE path, so its locator has to be controlled in BOTH directions: the live
// value must be rewritten, and a commented one must not.
//
// The account id must never reach the public catalog — that is what the placeholder is
// for. Substituting inside a comment writes the real id into the operator's fork somewhere
// nothing reads and nobody looks, which is a leak rather than a rewrite.
func TestSubstituteAccountID_RewritesValuesAndLeavesCommentsAlone(t *testing.T) {
	in := "# superseded: arn:aws:iam::" + Placeholder + ":role/old\n" +
		"roleArn: arn:aws:iam::" + Placeholder + ":role/live\n"

	out, n := SubstituteAccountID(in, "111111111111")

	if n != 1 {
		t.Errorf("counted %d substitutions, want 1 — a comment hit inflates the number "+
			"reported to the operator and decides whether the file is written", n)
	}
	if !strings.Contains(out, "role/live") || strings.Contains(out, Placeholder+":role/live") {
		t.Errorf("the live value was not rewritten:\n%s", out)
	}
	if !strings.Contains(out, Placeholder+":role/old") {
		t.Errorf("the commented placeholder was rewritten — the real account id is now in a "+
			"comment in a public fork:\n%s", out)
	}
}

// A file whose only placeholder is commented must not be rewritten at all. Reporting a
// substitution over a file nothing functional changed in is how a count stops meaning
// anything.
func TestSubstituteAccountID_CommentOnlyFileIsUntouched(t *testing.T) {
	in := "# example: arn:aws:iam::" + Placeholder + ":role/x\nroleArn: arn:aws:iam::999999999999:role/y\n"

	out, n := SubstituteAccountID(in, "111111111111")

	if n != 0 {
		t.Errorf("counted %d, want 0 — nothing functional carries a placeholder here", n)
	}
	if out != in {
		t.Errorf("the file was rewritten with nothing to substitute:\n%s", out)
	}
}

// A '#' inside a quoted value is part of the value, not the start of a comment. Splitting
// naively would cut the line and leave the placeholder after it unsubstituted.
func TestSubstituteAccountID_HashInsideAQuotedValueIsNotAComment(t *testing.T) {
	in := `tag: "a#b"` + "\nroleArn: arn:aws:iam::" + Placeholder + ":role/live\n"

	out, n := SubstituteAccountID(in, "111111111111")

	if n != 1 {
		t.Fatalf("counted %d, want 1", n)
	}
	if !strings.Contains(out, `tag: "a#b"`) {
		t.Errorf("the quoted value was mangled:\n%s", out)
	}
}
