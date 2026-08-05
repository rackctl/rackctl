package config

import (
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"
)

// A retired field is one rackctl used to honour and no longer does, usually because the thing it
// switched on was deleted upstream.
//
// Deleting the Go field is the whole fix in a strict decoder. It is not the whole fix here:
// Load uses sigs.k8s.io/yaml, which routes through encoding/json and IGNORES keys with no
// matching field. So removing `Accelerators bool` on its own would take a config that says
//
//	addons:
//	  accelerators: true
//
// and do nothing with it, forever, without a word. That is precisely the failure the test guarding
// that field was written about — "the config said yes and the platform said nothing" — reproduced
// by the act of removing the feature rather than by a bug in it.
//
// A retired field is also not the same as an unknown one. rackctl does not reject unknown keys
// wholesale, and should not: a config carrying a typo or a forward-compatible key from a newer
// rackctl is the operator's business. A key rackctl ITSELF removed is different — it was valid,
// it meant something, and somebody wrote it on purpose expecting that meaning. It gets a named
// refusal saying what happened to it, not silence.
type retiredField struct {
	// path is the dotted YAML location, e.g. "addons.accelerators".
	path string
	// gone says what happened, in a sentence the operator can act on. It is printed verbatim, so
	// it names the upstream change rather than saying "removed".
	gone string
}

var retiredFields = []retiredField{
	{
		path: "addons.accelerators",
		gone: "the GPU/accelerator stack was deleted upstream (ledger O27) — the addons-accelerators " +
			"ApplicationSet in eks-gitops, the accelerator-pools component and its live roots in " +
			"eks-agent-platform, and landing-zone's enable_accelerators variable and cluster label. " +
			"Three layers of it were inert: the DRA chart named a release published by no registry, " +
			"its values described an AcceleratorPool CRD that exists nowhere, and the driver is " +
			"mutually exclusive with the gpu-operator that shipped beside it. The model path is " +
			"Bedrock. Remove this key",
	},
}

// checkRetired reports every retired key present in the raw document.
//
// It reads the YAML a second time, into a generic map, because by the time Load has decoded into
// Config the evidence is gone — that is the entire problem being solved. The cost is one extra
// parse of a file measured in kilobytes, on a command that is about to stand up an EKS cluster.
//
// A present-but-false key is still reported. `accelerators: false` is not harmless just because
// its value happens to match what now happens: the operator wrote a switch believing it had two
// positions, and silently keeping the one that agrees leaves them believing it still does.
func checkRetired(doc []byte) error {
	var raw map[string]any
	if err := yaml.Unmarshal(doc, &raw); err != nil {
		// Load's own decode reports parse errors with the filename attached; a second, worse
		// message about the same bytes helps nobody.
		return nil
	}

	var found []string
	for _, f := range retiredFields {
		if !present(raw, strings.Split(f.path, ".")) {
			continue
		}
		found = append(found, fmt.Sprintf("  %s\n      %s", f.path, f.gone))
	}
	if len(found) == 0 {
		return nil
	}
	return fmt.Errorf("this config sets %d field(s) rackctl no longer honours:\n\n%s\n\n"+
		"Nothing is wrong with the rest of the file. These are refused rather than ignored "+
		"because they were valid, they meant something, and a config that asks for a thing and "+
		"silently gets nothing is the failure this project spends most of its effort removing",
		len(found), strings.Join(found, "\n\n"))
}

// present walks a dotted path through the decoded document. It answers whether the KEY is there,
// not what it holds — a retired field is retired at either value.
func present(node any, path []string) bool {
	m, ok := node.(map[string]any)
	if !ok {
		return false
	}
	v, ok := m[path[0]]
	if !ok {
		return false
	}
	if len(path) == 1 {
		return true
	}
	return present(v, path[1:])
}
