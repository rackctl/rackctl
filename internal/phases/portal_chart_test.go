package phases

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/engine"
	"github.com/rackctl/rackctl/internal/exec"
)

// chartRefState puts a fake `helm` on PATH that answers `show values` with `values`, or
// exits non-zero when it is empty — the unreachable-registry case. The checkout path is
// a temp dir so a fallback is distinguishable from the published ref by its value alone.
func chartRefState(t *testing.T, values string) (*engine.State, string) {
	t.Helper()
	dir := t.TempDir()

	script := "#!/bin/sh\nexit 1\n"
	if values != "" {
		script = "#!/bin/sh\ncat <<'HELMEOF'\n" + values + "\nHELMEOF\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "helm"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	checkout := filepath.Join(dir, "portal-checkout")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	return &engine.State{
		Config: &config.Config{Org: config.Org{Name: "acme"}},
		Runner: exec.New(io.Discard), // NOT dry-run — the point is to exec
		Repos:  engine.Repos{Portal: checkout},
	}, checkout
}

// The values portal's chart declares once it can compose DATABASE_URL from the
// RDS-managed secret. Anything the phase --sets lives under one of these two.
const portalWiredValues = `externalSecret:
  enabled: false
tenantInfra:
  relational:
    host: ""
objectStore:
  bucket: ""
`

// Published chart 0.1.0, verbatim in shape: it knows objectStore and nothing about the
// tenant substrate. This is the case that broke phase 8 — reachable, wrong.
const portalUnwiredValues = `objectStore:
  bucket: ""
database:
  url: ""
`

func TestPortalChartRef_PrefersAPublishedChartThatCanCompose(t *testing.T) {
	st, _ := chartRefState(t, portalWiredValues)

	if got := portalChartRef(st); !strings.HasPrefix(got, "oci://") {
		t.Fatalf("a published chart declaring the tenant values is the one to install, got %q", got)
	}
}

// The bug. GHCR holding *a* portal chart says nothing about whether it holds one that
// understands externalSecret: 0.1.0 renders no ExternalSecret, and with no
// values.schema.json helm accepts every --set in silence. What kept that from being a
// running portal wired to nothing was a `required` in the chart being replaced.
func TestPortalChartRef_RejectsAPublishedChartThatDeclaresNoTenantValues(t *testing.T) {
	st, checkout := chartRefState(t, portalUnwiredValues)

	got := portalChartRef(st)
	if strings.HasPrefix(got, "oci://") {
		t.Fatalf("a chart with no externalSecret cannot compose DATABASE_URL — installing it "+
			"wires portal to nothing, got %q", got)
	}
	if st.Runner.Dir != checkout {
		t.Errorf("the fallback must run helm from the checkout, got Dir=%q", st.Runner.Dir)
	}
}

// A pin says which portal this install is. The published chart is a separate artifact on
// its own version line and cannot be resolved from a git ref, so reaching for it defeats
// the pin the same way a child Application tracking main defeats a pinned ApplicationSet.
func TestPortalChartRef_APinOutranksTheRegistry(t *testing.T) {
	st, checkout := chartRefState(t, portalWiredValues) // registry is fine, and must lose anyway
	st.Config.Versions.Portal = "platform-v2026.08.10"

	if got := portalChartRef(st); strings.HasPrefix(got, "oci://") {
		t.Fatalf("versions.portal pins a ref — the registry must not be consulted, got %q", got)
	}
	if st.Runner.Dir != checkout {
		t.Errorf("the pinned install must run helm from the pinned checkout, got Dir=%q", st.Runner.Dir)
	}
}

func TestPortalChartRef_FallsBackWhenTheRegistryIsUnreachable(t *testing.T) {
	st, checkout := chartRefState(t, "") // helm exits non-zero

	if got := portalChartRef(st); strings.HasPrefix(got, "oci://") {
		t.Fatalf("an unreachable registry must not stop the install, got %q", got)
	}
	if st.Runner.Dir != checkout {
		t.Errorf("expected the checkout, got Dir=%q", st.Runner.Dir)
	}
}
