package phases

import (
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/engine"
)

// The tenants map rackctl injects must be portal's alone, and must carry the CR's own
// declaration rather than a restatement of it.
func TestPortalTenants_RendersTheCRIntoTheVariableShape(t *testing.T) {
	raw := []byte(`apiVersion: platform.nanohype.dev/v1alpha1
kind: BudgetPolicy
metadata:
  name: portal
---
apiVersion: platform.nanohype.dev/v1alpha1
kind: Platform
metadata:
  name: portal
spec:
  datastores:
    - name: main
      kind: relational
      relational:
        minACU: 0.5
        maxACU: 4
        backupRetentionDays: 7
    - name: state
      kind: objectStore
      objectStore:
        versioning: true
`)
	ds, err := datastoresOf(raw, "portal")
	if err != nil {
		t.Fatalf("datastoresOf: %v", err)
	}
	if len(ds) != 2 {
		t.Fatalf("want 2 datastores, got %d: %v", len(ds), ds)
	}
	rel, _ := ds[0]["relational"].(map[string]any)
	if rel == nil {
		t.Fatalf("relational block missing: %v", ds[0])
	}
	// camelCase in the CR, snake_case in the variable. Getting this wrong is silent:
	// terraform's optional() fills the default and the ACU range you asked for is ignored.
	for _, k := range []string{"min_acu", "max_acu", "backup_retention_days"} {
		if _, ok := rel[k]; !ok {
			t.Errorf("relational.%s missing — the CR's camelCase was not mapped: %v", k, rel)
		}
	}
	if _, bad := rel["minACU"]; bad {
		t.Errorf("camelCase survived into the terraform variable: %v", rel)
	}
	if os, _ := ds[1]["object_store"].(map[string]any); os == nil {
		t.Errorf("objectStore was not mapped to object_store: %v", ds[1])
	}
}

// A CR with no datastores is a portal with no database, and the chart bundles none.
func TestPortalTenants_RefusesACRWithNoDatastores(t *testing.T) {
	raw := []byte("kind: Platform\nmetadata:\n  name: portal\nspec: {}\n")
	if _, err := datastoresOf(raw, "portal"); err == nil {
		t.Fatal("a Platform with no datastores must not render an empty substrate silently")
	}
}

// substrateOutput builds a tenant_datastores payload for the portal tenant.
func substrateOutput(t *testing.T, relational string) *engine.State {
	t.Helper()
	return &engine.State{Outputs: map[string]string{
		"tenant_datastores": `{"portal":{` + relational + `,
			"logstream":{"kind":"cache","arn":"arn:cache","endpoint":"cache.internal","secret_arn":"arn:sec","database":null},
			"state":{"kind":"objectStore","arn":"arn:s3","endpoint":"bucket-name","secret_arn":null,"database":null}}}`,
	}}
}

// The database name is read from the substrate, never assumed.
//
// tenant-substrate composes it as app_<datastore>, so portal's `main` store becomes
// app_main — and the chart's own default is the string "portal". Those disagree, and the
// disagreement is invisible until runtime: a DSN naming the wrong database is well-formed,
// resolves, connects and authenticates before failing on `database "portal" does not
// exist`. That reads as a missing database rather than an unpassed value, which is the
// whole reason the name travels rather than being re-derived here.
func TestReadPortalSubstrate_CarriesTheDatabaseNameFromTheSubstrate(t *testing.T) {
	st := substrateOutput(t, `"main":{"kind":"relational","arn":"arn:rds","endpoint":"db.internal","secret_arn":"arn:rds!cluster-x","database":"app_main"}`)

	sub, err := readPortalSubstrate(st)
	if err != nil {
		t.Fatalf("readPortalSubstrate: %v", err)
	}
	if sub.DBName != "app_main" {
		t.Errorf("database name must come from the substrate output, got %q — the chart default is "+
			"\"portal\" and would connect to a database that does not exist", sub.DBName)
	}
	if sub.DBHost != "db.internal" || sub.DBSecretARN != "arn:rds!cluster-x" {
		t.Errorf("endpoint/secret regressed: host=%q secret=%q", sub.DBHost, sub.DBSecretARN)
	}
}

// And an absent name is refused here rather than silently falling back to the chart's.
//
// A component predating the `database` output publishes no such field, which unmarshals to
// "". Passing that through means --set with an empty value, so the chart keeps its default
// and the failure moves into the pod. Failing in the phase that read the output puts the
// error next to the thing that can fix it.
func TestReadPortalSubstrate_RefusesAnAbsentDatabaseName(t *testing.T) {
	st := substrateOutput(t, `"main":{"kind":"relational","arn":"arn:rds","endpoint":"db.internal","secret_arn":"arn:rds!cluster-x"}`)

	_, err := readPortalSubstrate(st)
	if err == nil {
		t.Fatal("a relational datastore with no database name must fail the phase, not fall through " +
			"to the chart's default")
	}
	if !strings.Contains(err.Error(), "database name") {
		t.Errorf("the error must name what is missing:\n%v", err)
	}
}
