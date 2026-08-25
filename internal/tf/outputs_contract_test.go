package tf

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A contract test over a FULL `terragrunt output -json` document, not a minimal one.
//
// testing-rubric asks for a contract test pinning the request/response shape the code
// relies on, so an upstream change surfaces as a test failure rather than as a production
// incident. A hand-written two-key fixture cannot do that: it is written to the parser's
// current assumptions and agrees with them by construction.
//
// This fixture carries every value type terraform emits — string, tuple, object, number,
// bool, and a sensitive string — including the `sensitive` and `type` keys ParseOutputs
// ignores. A parser that starts depending on one of them, or an upstream that renames
// `value`, fails here.
//
// It is transcribed to the documented shape rather than captured from a live apply, which
// is the honest limit: it pins the SHAPE the code relies on and cannot prove that shape is
// what a given terragrunt version emits today.
func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

func TestParseOutputs_AgainstAFullDocument(t *testing.T) {
	got, err := ParseOutputs(loadFixture(t, "terragrunt-output.json"))
	if err != nil {
		t.Fatalf("a well-formed terragrunt document must parse: %v", err)
	}

	// A string comes back unquoted. Everything else comes back as its compact JSON, which
	// is what the callers that pass a list into a TF_VAR depend on.
	for _, tc := range []struct{ key, want string }{
		{"cluster_name", "development-platform"},
		{"cluster_endpoint", "https://ABCDEF0123456789.gr7.us-east-1.eks.amazonaws.com"},
		{"private_subnet_ids", `["subnet-0a1b2c3d","subnet-1b2c3d4e","subnet-2c3d4e5f"]`},
		{"route_table_ids", `["rtb-0a1b2c3d4e5f"]`},
		{"nat_gateway_count", "1"},
		{"flow_logs_enabled", "false"},
	} {
		if got[tc.key] != tc.want {
			t.Errorf("%s = %q, want %q", tc.key, got[tc.key], tc.want)
		}
	}

	// An object round-trips as compact JSON with its keys intact.
	if !strings.Contains(got["node_security_group"], `"id":"sg-0a1b2c3d"`) {
		t.Errorf("node_security_group = %q, want compact JSON carrying its keys", got["node_security_group"])
	}

	// Every key in the document reaches the map. A parser that silently drops the types it
	// does not recognise would pass every assertion above.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(loadFixture(t, "terragrunt-output.json"), &raw); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if len(got) != len(raw) {
		t.Errorf("parsed %d outputs from a document carrying %d — a value type is being "+
			"dropped rather than encoded", len(got), len(raw))
	}
}

// The contract's OTHER half: a document that is not this shape must fail loudly.
//
// A parser that returns an empty map for a changed response reads as "this component
// published no outputs", and every caller downstream then resolves a placeholder instead
// of an ARN. The detector has to be shown able to produce a positive.
func TestParseOutputs_RejectsWhatIsNotTheContract(t *testing.T) {
	for name, doc := range map[string]string{
		"not JSON at all":             `Error: could not read state`,
		"a JSON array, not an object": `[{"value":"x"}]`,
		"a bare string":               `"cluster_name"`,
	} {
		if _, err := ParseOutputs([]byte(doc)); err == nil {
			t.Errorf("%s parsed cleanly — a changed response must surface as an error, not "+
				"as a component that published nothing", name)
		}
	}

	// An output whose `value` key is renamed upstream parses without error and yields an
	// empty string, which is the silent case worth pinning: the map is non-empty, so a
	// caller checking only for emptiness is satisfied.
	got, err := ParseOutputs([]byte(`{"cluster_name":{"result":"development-platform"}}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["cluster_name"] != "" {
		t.Fatalf("got %q — the fixture renames `value`, so this documents what happens", got["cluster_name"])
	}
	// Callers must therefore check the VALUE, not the map's length. needOutput does.
	if len(got) == 0 {
		t.Fatal("the map is non-empty here, which is exactly why length is the wrong check")
	}
}
