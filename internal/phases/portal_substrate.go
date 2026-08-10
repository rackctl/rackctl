package phases

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/rackctl/rackctl/internal/engine"
)

// portalTenant is the tenant id portal's Platform CR declares, and the key every AWS
// resource tenant-substrate builds for it is named from.
const portalTenant = "portal"

// platformCR is the sliver of a Platform CR this needs: the datastore declarations.
// Deliberately not the whole schema — rackctl is not a second implementation of the CRD,
// and a field it does not read is a field it cannot get wrong.
type platformCR struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Datastores []map[string]any `json:"datastores"`
	} `json:"spec"`
}

// portalTenantsVar renders TF_VAR_tenants for tenant-substrate from portal's own
// Platform CR, and from nothing else.
//
// WHY IT IS PORTAL-ONLY, which is the whole design decision here.
//
// tenant-substrate reads `tenants` from the environment's committed
// tenants.generated.json — every tenant APP that environment carries. Applying that map
// from an installer would provision four unrelated Aurora clusters on the way to standing
// up a console. Those apps are a GitOps concern; their substrate arrives with them.
//
// An ambient TF_VAR beats a leaf's `inputs` (the one terragrunt precedence rule this repo
// depends on most), so rackctl supplies a map containing exactly the tenants the PLATFORM
// owns. The committed selection stays what it is, and this run neither provisions nor
// destroys substrate belonging to an application nobody asked it to deploy.
//
// The declaration is read from the CR rather than restated here. Restating it would put
// portal's datastore shape in two places, and the second one would be the one nobody
// updates.
func portalTenantsVar(st *engine.State) (string, error) {
	path := filepath.Join(st.Repos.Portal, "platform.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		// A dry-run does not clone, so the CR is not on disk and cannot be. Failing here
		// would make `rackctl plan` refuse a config that applies perfectly — the same
		// reason needOutput returns a placeholder rather than an error under DryRun. The
		// value is deliberately not a plausible map: it appears in the printed TF_VAR so
		// the reader can see this is the shape of the run, not a resolved one.
		if st.Runner.DryRun {
			return `{"portal":{"datastores":"<portal/platform.yaml spec.datastores>"}}`, nil
		}
		return "", fmt.Errorf("reading portal's Platform CR at %s: %w", path, err)
	}

	ds, err := datastoresOf(raw, portalTenant)
	if err != nil {
		return "", err
	}
	blob, err := json.Marshal(map[string]any{
		portalTenant: map[string]any{"datastores": ds},
	})
	if err != nil {
		return "", fmt.Errorf("marshalling portal's tenants map: %w", err)
	}
	return string(blob), nil
}

// datastoresOf pulls one Platform's spec.datastores out of a multi-document CR file and
// converts it to tenant-substrate's variable shape.
//
// The two schemas differ only in case convention — the CR is camelCase because it is
// Kubernetes, the variable is snake_case because it is Terraform — which is exactly what
// landing-zone's own scripts/render-tenants.py documents. This is the same mapping for the
// one tenant rackctl owns; the renderer remains the source of truth for the committed map.
func datastoresOf(raw []byte, name string) ([]map[string]any, error) {
	for _, doc := range splitYAML(raw) {
		var cr platformCR
		if err := yaml.Unmarshal(doc, &cr); err != nil {
			continue // not every document in the file is a Platform
		}
		if cr.Kind != "Platform" || cr.Metadata.Name != name {
			continue
		}
		if len(cr.Spec.Datastores) == 0 {
			return nil, fmt.Errorf("portal's Platform CR declares no spec.datastores — there would "+
				"be nothing for tenant-substrate to provision, and portal bundles no database, "+
				"cache or object store of its own (%s)", name)
		}
		out := make([]map[string]any, 0, len(cr.Spec.Datastores))
		for _, d := range cr.Spec.Datastores {
			out = append(out, toSnake(d))
		}
		return out, nil
	}
	return nil, fmt.Errorf("no Platform named %q in portal's platform.yaml — the tenant id, the "+
		"CR's metadata.name and the key every AWS resource is named from are all one string, and "+
		"they have to match", name)
}

// crToTF maps the CR's camelCase datastore vocabulary onto the variable's snake_case.
// Only the keys that differ; anything else passes through unchanged.
var crToTF = map[string]string{
	"deletionPolicy":         "deletion_policy",
	"objectStore":            "object_store",
	"keyValue":               "key_value",
	"engineVersion":          "engine_version",
	"minACU":                 "min_acu",
	"maxACU":                 "max_acu",
	"backupRetentionDays":    "backup_retention_days",
	"deletionProtection":     "deletion_protection",
	"lifecycleExpireDays":    "lifecycle_expire_days",
	"nodeType":               "node_type",
	"partitionKey":           "partition_key",
	"sortKey":                "sort_key",
	"billingMode":            "billing_mode",
	"ttlAttribute":           "ttl_attribute",
	"pointInTimeRecovery":    "point_in_time_recovery",
	"globalSecondaryIndexes": "global_secondary_indexes",
}

func toSnake(v any) map[string]any {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, val := range m {
		key := k
		if mapped, ok := crToTF[k]; ok {
			key = mapped
		}
		switch t := val.(type) {
		case map[string]any:
			out[key] = toSnake(t)
		case []any:
			list := make([]any, 0, len(t))
			for _, e := range t {
				if em, ok := e.(map[string]any); ok {
					list = append(list, toSnake(em))
				} else {
					list = append(list, e)
				}
			}
			out[key] = list
		default:
			out[key] = val
		}
	}
	return out
}

// splitYAML splits a multi-document YAML file on its document separators.
//
// Naive on purpose: the separator is matched at the start of a line, which is where a
// document separator is, and a `---` inside a block scalar would fool it. Portal's CR has
// none, and the alternative is a streaming decoder for a file this reads three fields out
// of.
func splitYAML(raw []byte) [][]byte {
	var docs [][]byte
	start := 0
	lines := splitLines(raw)
	var cur []byte
	for _, l := range lines {
		if string(l) == "---" {
			if len(cur) > 0 {
				docs = append(docs, cur)
			}
			cur = nil
			continue
		}
		cur = append(cur, l...)
		cur = append(cur, '\n')
	}
	_ = start
	if len(cur) > 0 {
		docs = append(docs, cur)
	}
	return docs
}

func splitLines(raw []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, b := range raw {
		if b == '\n' {
			out = append(out, raw[start:i])
			start = i + 1
		}
	}
	if start < len(raw) {
		out = append(out, raw[start:])
	}
	return out
}

// portalDatastore is one entry of tenant-substrate's `tenant_datastores` output.
type portalDatastore struct {
	Kind      string `json:"kind"`
	ARN       string `json:"arn"`
	Endpoint  string `json:"endpoint"`
	SecretARN string `json:"secret_arn"`
}

// portalSubstrate is what portal's chart needs from the substrate that was just built.
type portalSubstrate struct {
	DBHost      string
	DBSecretARN string
	CacheHost   string
	CacheSecret string
	Bucket      string
}

// readPortalSubstrate pulls portal's endpoints and its RDS master-secret ARN out of
// tenant-substrate's output.
//
// The secret ARN is the one value that cannot come from anywhere else. RDS names the
// master credential itself — rds!cluster-<id> — so there is no name to compose and no
// convention to follow; the only way to know it is to read it from the thing that made it.
// The endpoints could in principle be derived, and are not, for the same reason: a derived
// endpoint that is subtly wrong fails at connect time, hours later, in the app.
func readPortalSubstrate(st *engine.State) (*portalSubstrate, error) {
	raw := st.Outputs["tenant_datastores"]
	if raw == "" {
		return nil, fmt.Errorf("tenant-substrate published no tenant_datastores — portal's Postgres, " +
			"Redis and bucket are declared in its Platform CR and provisioned by that component, so " +
			"without this there is nothing to point the chart at")
	}
	var byTenant map[string]map[string]portalDatastore
	if err := json.Unmarshal([]byte(raw), &byTenant); err != nil {
		return nil, fmt.Errorf("parsing tenant_datastores: %w", err)
	}
	stores, ok := byTenant[portalTenant]
	if !ok {
		return nil, fmt.Errorf("tenant_datastores has no %q entry — the tenant id, portal's Platform "+
			"CR metadata.name and the TF_VAR_tenants key are one string and must match", portalTenant)
	}

	out := &portalSubstrate{}
	for name, d := range stores {
		switch d.Kind {
		case "relational":
			out.DBHost, out.DBSecretARN = d.Endpoint, d.SecretARN
		case "cache":
			out.CacheHost, out.CacheSecret = d.Endpoint, d.SecretARN
		case "objectStore":
			out.Bucket = d.Endpoint // the module reports a bucket's name as its endpoint
		default:
			note(st, "portal: ignoring datastore %q of kind %q — the chart consumes relational, "+
				"cache and objectStore", name, d.Kind)
		}
	}
	// Postgres is the only one portal cannot start without: the domain model AND River's
	// job queue live in it. Redis is optional in the chart (the log streamer degrades to
	// in-memory) and the bucket only gates state storage, so neither is fatal here.
	if out.DBHost == "" || out.DBSecretARN == "" {
		return nil, fmt.Errorf("portal's relational datastore reported endpoint=%q secret_arn=%q — "+
			"both are required, and portal bundles no database to fall back on",
			out.DBHost, out.DBSecretARN)
	}
	return out, nil
}

// applyPortalPlatform applies portal's own Platform CR and waits for the one object the
// chart cannot start without.
//
// It waits on the tenant-runtime ServiceAccount rather than on Platform.status.phase, and
// the difference is the point: the phase is the operator's summary of its own work, while
// the ServiceAccount is the thing a pod's admission actually resolves. Those come apart —
// a Platform can report Ready while a later reconcile step has not landed — and this stack
// has produced that failure often enough that the concrete object is the safer assertion.
//
// The wait is generous because the operator reconciles the namespace, the quota, the
// NetworkPolicy, the AppProject and an IAM role with a Pod Identity association before the
// account is useful, and the IAM half is an AWS round trip.
func applyPortalPlatform(ctx context.Context, st *engine.State, ns string) error {
	cr := filepath.Join(st.Repos.Portal, "platform.yaml")
	if _, err := os.Stat(cr); err != nil {
		return fmt.Errorf("portal's Platform CR is not at %s: %w — the chart's pods run as the "+
			"tenant-runtime ServiceAccount the operator creates from it, so without this there is "+
			"no identity to run as", cr, err)
	}

	note(st, "portal: applying %s — the Platform the operator derives %s and its tenant-runtime "+
		"ServiceAccount from", cr, ns)
	if err := st.Runner.Run(ctx, "kubectl", "apply", "-f", cr); err != nil {
		return fmt.Errorf("applying portal's Platform CR: %w", err)
	}
	if st.Runner.DryRun {
		return nil
	}
	return waitTenantServiceAccount(ctx, st, ns, tenantRuntimeSA, 5*time.Minute)
}

// tenantRuntimeSA is the ServiceAccount eks-agent-platform's operator creates in every
// non-vcluster tenant namespace, and the subject its Pod Identity association names.
const tenantRuntimeSA = "tenant-runtime"

func waitTenantServiceAccount(ctx context.Context, st *engine.State, ns, sa string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := st.Runner.Capture(ctx, "kubectl", "-n", ns, "get", "serviceaccount", sa,
			"-o", "name", "--request-timeout=30s"); err == nil {
			note(st, "portal: %s/%s exists — the operator has reconciled the tenant boundary", ns, sa)
			return nil
		}
		// Capped by whatever is left, so the loop honours its deadline rather than
		// overshooting it by a whole poll interval.
		wait := min(10*time.Second, time.Until(deadline))
		if wait <= 0 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}

	// Report what the operator thinks, because "the ServiceAccount is absent" says nothing
	// about why. A Platform stuck Pending names its own reason; one that never appeared at
	// all means the apply landed somewhere the controller is not watching.
	status, _ := st.Runner.Capture(ctx, "kubectl", "-n", portalControlPlaneNS, "get", "platform", "portal",
		"-o", `jsonpath={.status.phase}{" "}{range .status.conditions[*]}{.type}={.status}({.reason}) {end}`)
	if strings.TrimSpace(status) == "" {
		status = "<no Platform named portal in " + portalControlPlaneNS + ">"
	}
	return fmt.Errorf("%s/%s never appeared within %s — portal's pods run as it, so they would "+
		"stay unschedulable. Platform status: %s", ns, sa, timeout, status)
}

// portalControlPlaneNS is where portal's Platform and BudgetPolicy live: the CONTROL-PLANE
// namespace of the tenant that owns them, not the workload namespace the operator derives
// from the Platform's name. platform.yaml pins both, and the two are easy to confuse.
const portalControlPlaneNS = "tenants-platform-ops"
