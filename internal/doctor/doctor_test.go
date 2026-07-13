package doctor

import "testing"

// Every fixture below is the shape of a real failure observed on a live cluster.
// The point of these tests is that the doctor is provably able to see them — the
// previous doctor could not see any of them and still reported success.

func TestSameRepo_NormalizesGitURLForms(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"https://github.com/acme/eks-gitops.git", "github.com/acme/eks-gitops", true},
		{"https://github.com/acme/eks-gitops", "https://github.com/acme/eks-gitops.git", true},
		{"git@github.com:acme/eks-gitops.git", "https://github.com/acme/eks-gitops", true},
		{"https://github.com/ACME/EKS-Gitops.git", "github.com/acme/eks-gitops", true},
		{"https://github.com/acme/eks-gitops/", "github.com/acme/eks-gitops", true},

		// the bug: the cluster syncs from the UPSTREAM catalog, not the org's fork.
		// These must NOT compare equal, or the check that catches it is worthless.
		{"https://github.com/nanohype/eks-gitops.git", "https://github.com/acme/eks-gitops.git", false},
		{"https://github.com/acme/eks-gitops.git", "https://github.com/acme/other-repo.git", false},
	} {
		if got := sameRepo(tc.a, tc.b); got != tc.want {
			t.Errorf("sameRepo(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// A short consolidation window PLUS a multi-node budget is the combination that
// turns routine consolidation into a fleet-wide eviction. Either alone is fine.
func TestIsShortDuration(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"30s", true},  // the sandbox pool's setting
		{"1m", true},   // the setting that caused the storms
		{"4m", true},   //
		{"5m", false},  // boundary — 5m is not a hair trigger
		{"15m", false}, // the fixed value
		{"1h", false},
		{"", false},
		{"garbage", false},
	} {
		if got := isShortDuration(tc.in); got != tc.want {
			t.Errorf("isShortDuration(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestFailed(t *testing.T) {
	if Failed([]Result{ok("a", ""), warn("b", ""), skip("c", "")}) {
		t.Error("Failed() must be false when nothing failed — a Warn is not a failure")
	}
	if !Failed([]Result{ok("a", ""), fail("b", "")}) {
		t.Error("Failed() must be true when any check failed")
	}
}

func TestStatusString(t *testing.T) {
	for s, want := range map[Status]string{OK: "ok", Warn: "warn", Fail: "fail", Skip: "skip"} {
		if got := s.String(); got != want {
			t.Errorf("Status(%d).String() = %q, want %q", s, got, want)
		}
	}
}

func TestTruncate(t *testing.T) {
	// Condition messages are long and multi-line; they must collapse to one line.
	long := "Dashboard failed to be applied for 1 out of 1 instances. Errors:\n- grafana-operator/external: [POST /dashboards/db][500]"
	got := truncate(long, 40)
	if n := len([]rune(got)); n > 41 { // 40 + the ellipsis
		t.Errorf("truncate did not bound length to 40 runes, got %d: %q", n, got)
	}
	for _, r := range got {
		if r == '\n' {
			t.Errorf("truncate left a newline in %q", got)
		}
	}
	if truncate("short", 40) != "short" {
		t.Error("truncate must leave short strings alone")
	}
}

// Condition messages carry em-dashes and smart quotes. Slicing bytes rather than
// runes lands mid-rune and emits invalid UTF-8 — cut exactly on a multi-byte char.
func TestTruncate_DoesNotSplitRunes(t *testing.T) {
	s := "aaaa—bbbb" // the em-dash is 3 bytes, at rune index 4
	for n := 1; n < len([]rune(s)); n++ {
		got := truncate(s, n)
		if !utf8ValidString(got) {
			t.Fatalf("truncate(%q, %d) produced invalid UTF-8: %q", s, n, got)
		}
	}
}

func utf8ValidString(s string) bool {
	for _, r := range s {
		if r == '�' { // RuneError: a byte sequence that isn't a valid rune
			return false
		}
	}
	return true
}
