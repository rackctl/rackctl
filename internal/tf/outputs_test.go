package tf

import "testing"

func TestParseOutputs(t *testing.T) {
	data := []byte(`{
		"account_id": {"value": "123456789012", "type": "string"},
		"role_arns":  {"value": ["a","b"], "type": ["list","string"]}
	}`)
	m, err := ParseOutputs(data)
	if err != nil {
		t.Fatal(err)
	}
	if m["account_id"] != "123456789012" {
		t.Fatalf("account_id = %q, want 123456789012", m["account_id"])
	}
	if m["role_arns"] != `["a","b"]` {
		t.Fatalf("role_arns = %q, want compact JSON array", m["role_arns"])
	}
	if _, err := ParseOutputs([]byte("not json")); err == nil {
		t.Fatal("expected error on invalid JSON")
	}
}
