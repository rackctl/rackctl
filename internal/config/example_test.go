package config

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// examplePath is the shipped config the README points operators at as "the full config
// surface", and which the dns phase notes an install has used verbatim.
func examplePath(t *testing.T) string {
	t.Helper()
	p := filepath.Join("..", "..", "examples", "rackctl.yaml")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("the shipped example is missing: %v", err)
	}
	return p
}

// The example must LOAD. It is the first file an operator copies, so a parse error in it
// is a parse error in every install that starts from it — and nothing else in the suite
// reads this file at all.
func TestExample_Loads(t *testing.T) {
	cfg, err := Load(examplePath(t))
	if err != nil {
		t.Fatalf("the shipped example does not load, so neither does any config copied "+
			"from it: %v", err)
	}
	if cfg.Cluster.Name == "" {
		t.Error("the example must carry a cluster.name — it is required and has no default")
	}
}

// It must also VALIDATE. Loading proves the YAML parses; validating proves the values it
// demonstrates are ones rackctl accepts. An example that parses and would be refused at
// apply teaches a shape the tool rejects.
func TestExample_Validates(t *testing.T) {
	cfg, err := Load(examplePath(t))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the shipped example does not validate, so an operator copying it is "+
			"refused at apply: %v", err)
	}
}

// Every value the example presents as matching a default must actually match it.
//
// The example is a SECOND COPY of values that live in Default(), annotated with claims
// about what they mirror. A second copy drifts silently: the Go default moves, the example
// keeps saying "Matches landing-zone's default" beside the old number, and an operator
// copying it gets a value the comment promises is the default and is not.
func TestExample_AgreesWithTheDefaults(t *testing.T) {
	cfg, err := Load(examplePath(t))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	d := Default()

	for _, tc := range []struct {
		field     string
		got, want string
	}{
		{"cluster.version", cfg.Cluster.Version, d.Cluster.Version},
		{"cluster.network.vpcCidr", cfg.Cluster.Network.VPCCIDR, d.Cluster.Network.VPCCIDR},
		{"observability.tier", string(cfg.Observability.Tier), string(d.Observability.Tier)},
	} {
		if tc.got != tc.want {
			t.Errorf("%s is %q in the example and %q in Default() — the example annotates "+
				"this as a default, so the two must agree or the annotation is false",
				tc.field, tc.got, tc.want)
		}
	}
}

// The example must not hand an operator a real account id, region or org from somebody's
// estate. It is a template: the values are the shape, and a real one presented as the
// product's shape is a defect in an example exactly as in a default.
func TestExample_CarriesNoRealEstateValues(t *testing.T) {
	b, err := os.ReadFile(examplePath(t))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(b)

	for _, m := range regexp.MustCompile(`\b\d{12}\b`).FindAllString(body, -1) {
		if !isPlaceholderAccount(m) {
			t.Errorf("the example carries %q, which looks like a real account id — an "+
				"estate value presented as the product's shape", m)
		}
	}
}

// isPlaceholderAccount reports whether a twelve-digit string is obviously not an account.
//
// The SHAPE, not a list of known placeholders: enumerating prefixes means the next
// placeholder anyone writes is read as a real account, and the check that was supposed to
// find estate values instead finds its own examples. A repeated digit or a run of
// consecutive digits is a placeholder; anything else is treated as real.
func isPlaceholderAccount(s string) bool {
	allSame, sequential := true, true
	for i := 1; i < len(s); i++ {
		if s[i] != s[0] {
			allSame = false
		}
		// Modulo ten, so 123456789012 — which wraps at the nine — is still recognised.
		// Without the wrap the detector accepts it as real, which is the direction that
		// costs: a placeholder read as an estate value fails a clean tree.
		if s[i]-'0' != (s[i-1]-'0'+1)%10 {
			sequential = false
		}
	}
	return allSame || sequential
}

// The detector must be able to produce a POSITIVE, or a clean sweep says nothing.
//
// "I looked and found nothing" is worth exactly as much as the looking is worth, and a
// predicate that returns true for everything would pass the check above over an example
// full of real account ids. So it is shown to separate the two.
func TestIsPlaceholderAccount_SeparatesPlaceholdersFromRealIDs(t *testing.T) {
	for _, s := range []string{"000000000000", "111111111111", "222222222222", "123456789012"} {
		if !isPlaceholderAccount(s) {
			t.Errorf("%q is a placeholder and must be accepted", s)
		}
	}
	// Real-shaped ids, none of them anyone's: the point is that the detector FIRES.
	for _, s := range []string{"477165397189", "351619759866", "908234110765"} {
		if isPlaceholderAccount(s) {
			t.Errorf("%q is account-shaped and must be flagged — a detector that accepts "+
				"everything makes the sweep above meaningless", s)
		}
	}
}
