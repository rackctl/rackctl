package phases

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/engine"
	"github.com/rackctl/rackctl/internal/exec"
)

// beforeSpendState builds the trees acquire would have cloned: a landing-zone checkout with
// a live root for every component cfg applies, an agent-platform checkout with a root for
// each of its components when roots is true, and portal's Platform CR when cr is non-empty.
func beforeSpendState(t *testing.T, cfg *config.Config, roots bool, cr string) *engine.State {
	t.Helper()
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	st := &engine.State{Config: cfg, Runner: exec.New(io.Discard), Repos: engine.Repos{
		LandingZone: t.TempDir(), AgentPlatform: t.TempDir(), Portal: t.TempDir(),
	}}
	if err := os.MkdirAll(filepath.Join(st.Repos.LandingZone, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range CoreComponents(cfg) {
		write(filepath.Join(st.Repos.LandingZone, componentDir(st, c), "terragrunt.hcl"), "inputs = {}\n")
	}
	if roots {
		for _, c := range agentPlatformComponents(cfg) {
			write(filepath.Join(st.Repos.AgentPlatform, agentPlatformDir(st, c), "terragrunt.hcl"), "inputs = {}\n")
		}
	}
	if cr != "" {
		write(filepath.Join(st.Repos.Portal, "platform.yaml"), cr)
	}
	return st
}

func beforeSpendCfg() *config.Config {
	cfg := config.Default()
	cfg.Org.Name = "acme"
	cfg.Cloud.AccountID = "111111111111"
	cfg.Cluster.Name = "platform"
	return cfg
}

func isRefusal(err error) bool {
	var nr *engine.NoRollbackError
	return errors.As(err, &nr) && nr.BeforeSpend
}

// An agent-platform root missing is found where it costs nothing. Phase 6 found it with the
// VPC and the EKS control plane already built.
func TestBeforeSpend_AMissingAgentPlatformRootRefuses(t *testing.T) {
	st := beforeSpendState(t, beforeSpendCfg(), false, "")
	err := assertBeforeSpend(st)
	if !isRefusal(err) {
		t.Fatalf("a missing agent-platform root must refuse before spend, got %v", err)
	}
	if !strings.Contains(err.Error(), "bedrock-account") {
		t.Errorf("the refusal must name the missing root: %v", err)
	}
}

// With every root present, and the agent platform off, nothing refuses.
func TestBeforeSpend_PresentRootsPass(t *testing.T) {
	if err := assertBeforeSpend(beforeSpendState(t, beforeSpendCfg(), true, "")); err != nil {
		t.Fatalf("every root is present: %v", err)
	}
	cfg := beforeSpendCfg()
	off := false
	cfg.AgentPlatform.Enable = &off
	if err := assertBeforeSpend(beforeSpendState(t, cfg, false, "")); err != nil {
		t.Fatalf("the agent platform is off, so its tree is not asked about: %v", err)
	}
}

const portalCR = `kind: Platform
metadata:
  name: portal
spec:
  datastores:
    - name: main
      kind: relational
      relational:
        minACU: 0.5
        %s
`

// A datastore key the rendering cannot map is refused before any tofu run. Left to phase 4
// it passed through verbatim and terraform's typed object dropped it without a word.
func TestBeforeSpend_AnUnmappedDatastoreKeyRefuses(t *testing.T) {
	cfg := beforeSpendCfg()
	cfg.ControlPlane.Portal = true
	st := beforeSpendState(t, cfg, true, strings.Replace(portalCR, "%s", "performanceInsights: true", 1))
	err := assertBeforeSpend(st)
	if !isRefusal(err) {
		t.Fatalf("an unmapped key must refuse before spend, got %v", err)
	}
	if !strings.Contains(err.Error(), "spec.datastores[0].relational.performanceInsights") {
		t.Errorf("the refusal must name the key by its path: %v", err)
	}
}

// A plan reports the same finding and carries on: its tree may be older than the apply's.
func TestBeforeSpend_APlanNotesAnUnmappedKey(t *testing.T) {
	cfg := beforeSpendCfg()
	cfg.ControlPlane.Portal = true
	st := beforeSpendState(t, cfg, true, strings.Replace(portalCR, "%s", "performanceInsights: true", 1))
	var out strings.Builder
	st.Runner = exec.New(&out)
	st.Runner.DryRun = true
	if err := assertBeforeSpend(st); err != nil {
		t.Fatalf("a plan notes, it does not refuse: %v", err)
	}
	if !strings.Contains(out.String(), "performanceInsights") {
		t.Errorf("the plan must say what the apply would refuse:\n%s", out.String())
	}
}

// A CR whose keys all map passes.
func TestBeforeSpend_AMappedCRPasses(t *testing.T) {
	cfg := beforeSpendCfg()
	cfg.ControlPlane.Portal = true
	st := beforeSpendState(t, cfg, true, strings.Replace(portalCR, "%s", "maxACU: 4", 1))
	if err := assertBeforeSpend(st); err != nil {
		t.Fatalf("every key maps: %v", err)
	}
}

// Keys at every level toSnake walks are checked, in lists too, and values never are.
func TestUnmappedKeys_EveryLevelAndKeysOnly(t *testing.T) {
	d := map[string]any{
		"kind": "objectStore",
		"keyValue": map[string]any{
			"partitionKey": "id",
			"globalSecondaryIndexes": []any{
				map[string]any{"indexName": "by-owner", "sortKey": "owner"},
			},
		},
		"objectStore": map[string]any{"Versioning": true},
	}
	got := strings.Join(unmappedKeys(d, "spec.datastores[0]"), " ")
	for _, want := range []string{
		"spec.datastores[0].keyValue.globalSecondaryIndexes[0].indexName",
		"spec.datastores[0].objectStore.Versioning",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("%s was not reported: %s", want, got)
		}
	}
	for _, mapped := range []string{".kind", "partitionKey", "sortKey", ".keyValue ", "globalSecondaryIndexes "} {
		if strings.Contains(got+" ", mapped+" ") {
			t.Errorf("%s is mapped or lowercase and must not be reported: %s", mapped, got)
		}
	}
	if unmappedKeys("not a map", "x") != nil {
		t.Error("a scalar has no keys")
	}
}

// The outputs phase 6 cannot run without are asserted where they are produced. Found in
// phase 6, a missing one cost the substrate and the GitOps phases first.
func TestClusterPhase_AMissingRequiredOutputFailsWhereItIsProduced(t *testing.T) {
	st := fakeAWSAndTG(t, 0)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "terragrunt"), []byte(`#!/bin/sh
if [ "$3" = "output" ]; then echo '{"vpc_id":{"value":"vpc-0abc"}}'; fi
exit 0
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	err := (cluster{}).Run(t.Context(), st)
	if err == nil || !strings.Contains(err.Error(), `"private_subnet_ids"`) || !strings.Contains(err.Error(), `"network"`) {
		t.Fatalf("the phase must fail naming the output and its producer, got %v", err)
	}
	var nr *engine.NoRollbackError
	if errors.As(err, &nr) {
		t.Errorf("nothing has converged, so this failure rolls back: %v", err)
	}
	if st.KubeconfigCluster != "" {
		t.Errorf("the phase must stop before repointing kubectl")
	}
}

func TestRequireOutputs(t *testing.T) {
	st := &engine.State{Config: beforeSpendCfg(), Runner: exec.New(io.Discard),
		Outputs: map[string]string{"kms_key_arn": "arn:aws:kms:us-west-2:111111111111:key/k"}}
	if err := requireOutputs(st, "secrets", "kms_key_arn"); err != nil {
		t.Errorf("a present output passes: %v", err)
	}
	if err := requireOutputs(st, "network", "vpc_id"); err == nil || !strings.Contains(err.Error(), "vpc_id") {
		t.Errorf("a missing output fails by name, got %v", err)
	}
	st.Runner.DryRun = true
	if err := requireOutputs(st, "network", "vpc_id"); err != nil {
		t.Errorf("a plan captures nothing and asserts nothing: %v", err)
	}
	st.Runner.DryRun = false
	off := false
	st.Config.AgentPlatform.Enable = &off
	if err := requireOutputs(st, "network", "vpc_id"); err != nil {
		t.Errorf("without the agent platform nothing reads these: %v", err)
	}
}

// The secrets CMK is asserted as soon as secrets is read, before the rest of the substrate.
func TestSubstrate_AMissingKMSKeyFailsBeforeTheNextComponent(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "tg.log")
	if err := os.WriteFile(filepath.Join(dir, "terragrunt"), []byte(`#!/bin/sh
echo "$*" >> `+log+`
if [ "$3" = "output" ]; then echo '{"other":{"value":"x"}}'; fi
exit 0
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg := beforeSpendCfg()
	st := &engine.State{Config: cfg, Runner: exec.New(io.Discard), Outputs: map[string]string{},
		Repos: engine.Repos{LandingZone: dir}}

	err := (substrate{}).Run(t.Context(), st)
	if err == nil || !strings.Contains(err.Error(), `"kms_key_arn"`) || !strings.Contains(err.Error(), `"secrets"`) {
		t.Fatalf("the phase must fail naming the CMK output and its producer, got %v", err)
	}
	b, _ := os.ReadFile(log)
	next := substrateComponents(cfg)[1]
	if strings.Contains(string(b), "/"+next+" ") {
		t.Errorf("%s was applied after the secrets CMK was found missing:\n%s", next, b)
	}
}
