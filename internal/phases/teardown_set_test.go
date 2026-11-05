package phases

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/engine"
	"github.com/rackctl/rackctl/internal/exec"
)

const lzKeyPrefix = "s3://111111111111-us-west-2-tfstate/development/aws/workload-development/us-west-2/development/"

// teardownState is a development State whose fake aws answers each landing-zone state read
// from states (component -> the body of the aws stub's arm for that key) and 404s the rest. It
// records every argv it is called with, and runs in dry-run, because the probe is a read and
// must run under --dry-run exactly as on a real destroy.
func teardownState(t *testing.T, cfg *config.Config, states map[string]string) (*engine.State, string, *strings.Builder) {
	t.Helper()
	dir := t.TempDir()
	logf := filepath.Join(dir, "argv")
	var arms strings.Builder
	for comp, body := range states {
		fmt.Fprintf(&arms, "  *%s%s/terraform.tfstate*) %s ;;\n", lzKeyPrefix, comp, body)
	}
	script := fmt.Sprintf("#!/bin/sh\necho \"$*\" >> %q\ncase \"$*\" in\n%s"+
		"  *) echo 'fatal error: An error occurred (404) when calling the HeadObject operation: Not Found' >&2; exit 1 ;;\n"+
		"esac\n", logf, arms.String())
	if err := os.WriteFile(filepath.Join(dir, "aws"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var out strings.Builder
	run := exec.New(&out)
	run.DryRun = true
	run.ReadAttempts = 1
	cfg.Environment = config.EnvDev
	cfg.Cloud.AccountID, cfg.Cloud.Region = "111111111111", "us-west-2"
	cfg.Cluster.Name = "platform"
	return &engine.State{Config: cfg, Runner: run}, logf, &out
}

func stateWith(resources string) string { return "echo '{\"resources\":[" + resources + "]}'" }

// A component the config turned off after applying it is still standing, so the destroy walks
// it, in apply order, and names it. Before, the walk was CoreComponents(today's config): druid
// and dns were never visited, kept billing, and the destroy said "platform destroyed".
func TestTeardownComponents_WalksWhatStateHoldsNotWhatTheConfigNames(t *testing.T) {
	st, logf, out := teardownState(t, baseCfg(), map[string]string{
		"druid": stateWith(`{"type":"aws_rds_cluster"},{"type":"aws_s3_bucket"}`),
		"dns":   stateWith(`{"type":"aws_route53_zone","instances":[{"attributes":{"name":"acme.example.com."}}]}`),
		// An object that exists with no resources is a root already destroyed.
		"model-import": stateWith(""),
	})
	set := TeardownComponents(context.Background(), st)

	want := slices.Clone(CoreComponents(st.Config))
	want = slices.Insert(want, slices.Index(want, "observability")+1, "druid", "dns")
	if !slices.Equal(set.Components, want) {
		t.Errorf("Components = %v, want %v — the config's set plus druid and dns, in apply order", set.Components, want)
	}
	if !slices.Equal(set.Unconfigured, []string{"druid", "dns"}) {
		t.Errorf("Unconfigured = %v, want [druid dns]", set.Unconfigured)
	}
	if set.Zone != "acme.example.com" || len(set.Unread) != 0 {
		t.Errorf("Zone = %q, Unread = %v; want the zone the dns state records and nothing unread", set.Zone, set.Unread)
	}
	for _, name := range []string{"druid", "dns"} {
		if !strings.Contains(out.String(), name+" is not in this config but its state holds") {
			t.Errorf("the destroy must name %s as torn down from state:\n%s", name, out)
		}
	}
	// The config's own components are walked without asking: a destroy over empty state
	// removes nothing, and that walk is what a destroy always did.
	argv, _ := os.ReadFile(logf)
	for _, c := range CoreComponents(st.Config) {
		if strings.Contains(string(argv), "/development/"+c+"/terraform.tfstate") {
			t.Errorf("%s is in the config and its state was read anyway:\n%s", c, argv)
		}
	}
}

// A dns component the config still names keeps its own zone; the state's is lent only when
// the config has none.
func TestTeardownComponents_KeepsTheConfiguredZone(t *testing.T) {
	cfg := baseCfg()
	cfg.DNS = &config.DNS{HostedZone: "acme.example.com"}
	st, _, _ := teardownState(t, cfg, nil)
	if set := TeardownComponents(context.Background(), st); set.Zone != "" || !slices.Contains(set.Components, "dns") {
		t.Errorf("a configured dns is walked with its own zone, got Zone %q in %v", set.Zone, set.Components)
	}
}

// State that could not be read is neither walked nor taken as absent: it is named, so the
// destroy cannot report a clean teardown over it.
func TestTeardownComponents_UnreadStateIsNamedNotAssumedEmpty(t *testing.T) {
	st, _, out := teardownState(t, baseCfg(), map[string]string{
		"druid":        "echo 'An error occurred (AccessDenied) when calling the GetObject operation' >&2; exit 1",
		"fleet-hub":    "echo 'not json'",
		"model-import": "echo 'An error occurred (NoSuchKey) when calling the GetObject operation' >&2; exit 1",
		"dns":          "printf ''",
	})
	set := TeardownComponents(context.Background(), st)
	if !slices.Equal(set.Unread, []string{"druid", "fleet-hub"}) {
		t.Errorf("Unread = %v, want [druid fleet-hub]", set.Unread)
	}
	for _, c := range []string{"druid", "fleet-hub", "model-import", "dns"} {
		if slices.Contains(set.Components, c) {
			t.Errorf("%s holds no state anyone could read and must not be walked: %v", c, set.Components)
		}
	}
	if !strings.Contains(out.String(), "AccessDenied") || !strings.Contains(out.String(), "not readable terraform state") {
		t.Errorf("each unread state must say why:\n%s", out)
	}
}

// A dns state with no zone in it leaves the zone unknown, and the dns destroy then says so
// on its own component rather than guessing a domain.
func TestTeardownComponents_ADNSStateWithNoZoneLendsNone(t *testing.T) {
	st, _, _ := teardownState(t, baseCfg(), map[string]string{
		"dns": stateWith(`{"type":"aws_acm_certificate"},{"type":"aws_route53_zone","instances":[{"attributes":{"name":""}}]}`),
	})
	if set := TeardownComponents(context.Background(), st); set.Zone != "" || !slices.Contains(set.Unconfigured, "dns") {
		t.Errorf("Zone = %q, Unconfigured = %v; want dns walked and no zone", set.Zone, set.Unconfigured)
	}
}

// The universe is derived from CoreComponents with every gate on, so a component added there
// is one a destroy probes for.
func TestAllCoreComponents_IsEveryComponentCoreComponentsCanReturn(t *testing.T) {
	all := allCoreComponents(baseCfg())
	for _, c := range []string{"network", "cluster", "secrets", "agent-iam", "managed-monitoring", "observability",
		"druid", "dns", "model-import", "fleet-hub", "tenant-substrate", "cluster-addons", "cluster-bootstrap"} {
		if !slices.Contains(all, c) {
			t.Errorf("the universe a destroy probes is missing %s: %v", c, all)
		}
	}
}

// --force-buckets permits a bucket-owning component the destroy reaches from state, or it
// would meet BucketNotEmpty on the destroy the flag exists to clear.
func TestPermitBucketTeardown_CoversComponentsTornDownFromState(t *testing.T) {
	st, _, out := teardownState(t, baseCfg(), nil)
	st.Config.Environment = config.EnvStaging
	if err := PermitBucketTeardown(context.Background(), st, "druid", "dns", "agent-iam"); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "apply druid with TF_VAR_force_destroy_buckets=true") {
		t.Errorf("druid, torn down from state, must be permitted:\n%s", got)
	}
	if strings.Count(got, "apply agent-iam with TF_VAR_force_destroy_buckets=true") != 1 {
		t.Errorf("a component already in the config's set is permitted once:\n%s", got)
	}
	if strings.Contains(got, "apply dns with") {
		t.Errorf("dns owns no force_destroy buckets:\n%s", got)
	}
}
