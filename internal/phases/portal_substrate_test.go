package phases

import "testing"

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
