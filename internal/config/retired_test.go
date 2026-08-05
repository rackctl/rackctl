package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCfg writes a minimal valid config plus whatever extra YAML the test needs.
func writeCfg(t *testing.T, extra string) string {
	t.Helper()
	doc := `org:
  name: acme
  gitops:
    eksGitopsRepo: github.com/acme/eks-gitops
cloud:
  provider: aws
  accountId: "111111111111"
  region: us-west-2
  profile: dev
environment: development
cluster:
  name: platform
` + extra
	p := filepath.Join(t.TempDir(), "rackctl.yaml")
	if err := os.WriteFile(p, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// A retired key must be REFUSED, not ignored.
//
// Load decodes with sigs.k8s.io/yaml, which routes through encoding/json and drops keys with no
// matching field. So deleting the Go field on its own would take `accelerators: true` and do
// nothing with it, silently, forever — reproducing "the config said yes and the platform said
// nothing" by the act of removing the feature rather than by a bug in it.
func TestLoad_RefusesARetiredField(t *testing.T) {
	_, err := Load(writeCfg(t, "addons:\n  accelerators: true\n"))
	if err == nil {
		t.Fatal("a config asking for a deleted feature loaded clean. The decoder ignores unknown " +
			"keys, so removing the field without refusing it turns the request into silence")
	}
	for _, want := range []string{"addons.accelerators", "O27", "Bedrock"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name %q — it has to say what happened to the key, not just "+
				"that it is gone:\n%v", want, err)
		}
	}
}

// Present-but-false is still refused. `accelerators: false` is not harmless just because its
// value agrees with what now happens: the operator wrote a switch believing it had two
// positions, and silently accepting the position that agrees leaves them believing it still does.
func TestLoad_RefusesARetiredFieldEvenWhenFalse(t *testing.T) {
	if _, err := Load(writeCfg(t, "addons:\n  accelerators: false\n")); err == nil {
		t.Fatal("accelerators: false was accepted. The key is retired at either value — a switch " +
			"that silently has one position is worse than one that is gone")
	}
}

// And a config that does NOT carry it must load. A retirement check that fires on healthy input
// is worse than no check, because it teaches the operator to work around the loader.
func TestLoad_LeavesAHealthyConfigAlone(t *testing.T) {
	if _, err := Load(writeCfg(t, "addons:\n  druid: false\n")); err != nil {
		t.Fatalf("a config with no retired keys must load: %v", err)
	}
	// Including one with no addons block at all.
	if _, err := Load(writeCfg(t, "")); err != nil {
		t.Fatalf("an absent addons block is not a retired key: %v", err)
	}
}

// rackctl does not reject unknown keys wholesale, and must not start here. A typo, or a
// forward-compatible key from a newer rackctl, is the operator's business; only a key rackctl
// ITSELF removed gets a named refusal.
func TestLoad_DoesNotRejectMerelyUnknownKeys(t *testing.T) {
	if _, err := Load(writeCfg(t, "addons:\n  acclerators: true\n")); err != nil {
		t.Fatalf("a misspelled or unrecognised key is not a retired one, and this loader has never "+
			"rejected unknown fields: %v", err)
	}
}
