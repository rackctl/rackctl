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
	dev := filepath.Join(addon, "values-dev.yaml")
	if err := os.WriteFile(dev, []byte("arn: arn:aws:iam::000000000000:role/cm"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A different env's file must NOT be touched.
	prod := filepath.Join(addon, "values-prod.yaml")
	if err := os.WriteFile(prod, []byte("arn: arn:aws:iam::000000000000:role/cm"), 0o644); err != nil {
		t.Fatal(err)
	}

	n, changed, err := WriteBack(dir, "dev", "123456789012")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("replacements = %d, want 1", n)
	}
	if len(changed) != 1 || filepath.Base(changed[0]) != "values-dev.yaml" {
		t.Fatalf("changed = %v, want [values-dev.yaml]", changed)
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
