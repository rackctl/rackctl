// Package tf parses Terraform/Terragrunt CLI output.
package tf

import "encoding/json"

type outputValue struct {
	Value json.RawMessage `json:"value"`
}

// ParseOutputs parses `terragrunt output -json` / `terraform output -json`, a
// map of name → {value, type}. String values are returned as-is; non-string
// values (lists, objects) are returned as their compact JSON encoding.
func ParseOutputs(data []byte) (map[string]string, error) {
	var raw map[string]outputValue
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		var s string
		if err := json.Unmarshal(v.Value, &s); err == nil {
			out[k] = s
		} else {
			out[k] = string(v.Value)
		}
	}
	return out, nil
}
