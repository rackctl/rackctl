// Package tf parses Terraform/Terragrunt CLI output.
package tf

import (
	"bytes"
	"encoding/json"
)

type outputValue struct {
	Value json.RawMessage `json:"value"`
}

// ParseOutputs parses `terragrunt output -json` / `terraform output -json`, a
// map of name → {value, type}. String values are returned unquoted; non-string
// values (lists, objects, numbers, bools) are returned as compact JSON.
//
// COMPACTED, not passed through. These values become TF_VAR_* on the next invocation, so
// the bytes matter: passing the source through verbatim makes the variable's contents a
// function of how the upstream chose to format its output, and a terragrunt release that
// pretty-prints would change every list rackctl injects without changing anything rackctl
// wrote. Compacting normalises that at the boundary.
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
			continue
		}
		var buf bytes.Buffer
		if err := json.Compact(&buf, v.Value); err != nil {
			// Unreachable for anything json.Unmarshal accepted above, but a value that
			// cannot be compacted is passed through rather than dropped: a missing output
			// resolves to a placeholder downstream, which is worse than an odd one.
			out[k] = string(v.Value)
			continue
		}
		out[k] = buf.String()
	}
	return out, nil
}
