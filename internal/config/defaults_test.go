package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// minimalYAML is the smallest config Validate accepts: every required field and nothing else.
const minimalYAML = `org:
  name: acme
cloud:
  accountId: "111111111111"
  region: us-west-2
  profile: workload-development
environment: %ENV%
cluster:
  name: platform
`

// loadYAML writes body to a temporary rackctl.yaml and loads it.
func loadYAML(t *testing.T, body string) (*Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "rackctl.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

func minimal(env Environment) string { return strings.ReplaceAll(minimalYAML, "%ENV%", string(env)) }

// trueBools walks v and returns the dotted Go path of every bool that is true, split into
// plain bools and *bool fields. Nil pointers to structs are skipped: an absent optional block
// has no defaults of its own.
func trueBools(v reflect.Value, path string, plain, ptr *[]string) {
	switch v.Kind() {
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			trueBools(v.Field(i), path+"."+f.Name, plain, ptr)
		}
	case reflect.Pointer:
		if v.IsNil() {
			return
		}
		if v.Elem().Kind() == reflect.Bool {
			if v.Elem().Bool() {
				*ptr = append(*ptr, path)
			}
			return
		}
		trueBools(v.Elem(), path, plain, ptr)
	case reflect.Bool:
		if v.Bool() {
			*plain = append(*plain, path)
		}
	}
}

// fieldByPath follows a dotted Go field path from trueBools.
func fieldByPath(v reflect.Value, path string) reflect.Value {
	for _, name := range strings.Split(strings.TrimPrefix(path, "."), ".") {
		for v.Kind() == reflect.Pointer {
			if v.IsNil() {
				return reflect.Value{}
			}
			v = v.Elem()
		}
		v = v.FieldByName(name)
	}
	return v
}

// Every boolean Default() holds true must still be true in a loaded config that leaves it
// out. A plain bool cannot do that — the decoder writes false for an omitted key and nothing
// can tell it from an explicit false — so a true default must be a *bool, and ApplyDefaults
// must fill it. endpointPublicAccess and compliance.soc2 were plain bools true in Default()
// and false in every config that omitted them.
func TestLoad_EveryTrueDefaultSurvivesAMinimalConfig(t *testing.T) {
	var plain, ptr []string
	trueBools(reflect.ValueOf(Default()).Elem(), "", &plain, &ptr)
	for _, p := range plain {
		t.Errorf("Config%s is a plain bool that defaults to true — an omitted key decodes to false and "+
			"cannot be told from an explicit false. Make it a *bool with an accessor", p)
	}
	if len(ptr) == 0 {
		t.Fatal("found no *bool true in Default(), so this test checks nothing")
	}

	cfg, err := loadYAML(t, minimal(EnvDev))
	if err != nil {
		t.Fatalf("the minimal config must load: %v", err)
	}
	for _, p := range ptr {
		got := fieldByPath(reflect.ValueOf(cfg), p)
		if !got.IsValid() || got.IsNil() || !got.Elem().Bool() {
			t.Errorf("Config%s is true in Default() but not true in a loaded config that omits it", p)
		}
	}
	if !cfg.AgentPlatform.Compliance.SOC2Enabled() {
		t.Error("an omitted compliance.soc2 must resolve to true")
	}
}

// What an omitted endpointPublicAccess means depends on the environment: public in
// development, where the kubectl and helm phases run from the operator's host, and private
// in staging and production. Production refuses only an explicit true, so a minimal
// production config validates.
func TestLoad_EndpointPublicAccessDefaultsByEnvironment(t *testing.T) {
	for env, want := range map[Environment]bool{EnvDev: true, EnvStaging: false, EnvProduction: false} {
		cfg, err := loadYAML(t, minimal(env))
		if err != nil {
			t.Fatalf("%s: a minimal config must load: %v", env, err)
		}
		if got := cfg.PublicEndpoint(); got != want {
			t.Errorf("%s: an omitted endpointPublicAccess resolved to %t, want %t", env, got, want)
		}
		if !cfg.IsDefaulted("cluster.endpointPublicAccess") {
			t.Errorf("%s: the endpoint posture was defaulted and must be reported as such", env)
		}
	}

	prod := minimal(EnvProduction) + "  endpointPublicAccess: true\n"
	if _, err := loadYAML(t, prod); err == nil || !strings.Contains(err.Error(), "cluster.endpointPublicAccess") {
		t.Errorf("an explicit public endpoint in production must be refused by name, got %v", err)
	}

	dev := minimal(EnvDev) + "  endpointPublicAccess: false\n"
	cfg, err := loadYAML(t, dev)
	if err != nil {
		t.Fatalf("an explicit private development endpoint must load: %v", err)
	}
	if cfg.PublicEndpoint() || cfg.IsDefaulted("cluster.endpointPublicAccess") {
		t.Error("an explicit false must stay false and must not be reported as defaulted")
	}
}

// region and environment are required. A region picked for the operator can sit outside
// what the organization's SCPs allow, and an environment picked for them decides the overlay,
// the sizing and the endpoint posture. Each refusal names the field.
func TestLoad_RegionAndEnvironmentAreRequired(t *testing.T) {
	noRegion := strings.Replace(minimal(EnvDev), "  region: us-west-2\n", "", 1)
	if _, err := loadYAML(t, noRegion); err == nil || !strings.Contains(err.Error(), "cloud.region is required") {
		t.Errorf("a config with no region must be refused by name, got %v", err)
	}
	noEnv := strings.Replace(minimal(EnvDev), "environment: development\n", "", 1)
	if _, err := loadYAML(t, noEnv); err == nil || !strings.Contains(err.Error(), "environment is required") {
		t.Errorf("a config with no environment must be refused by name, got %v", err)
	}
}

// A node-group size the file sets to 0 is a value the operator wrote. Defaulting it to 2
// hid a request EKS refuses behind one it accepts; it is refused by name instead. An omitted
// size is still defaulted, and reported.
func TestLoad_AnExplicitZeroNodeSizeIsRefusedNotDefaulted(t *testing.T) {
	for _, field := range []string{"minSize", "maxSize", "desiredSize"} {
		body := minimal(EnvDev) + "  systemNodes:\n    " + field + ": 0\n"
		_, err := loadYAML(t, body)
		if err == nil || !strings.Contains(err.Error(), "cluster.systemNodes.") {
			t.Errorf("an explicit %s: 0 must be refused naming the node-group field, got %v", field, err)
		}
	}
	cfg, err := loadYAML(t, minimal(EnvDev))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Cluster.SystemNodes.MinSize != 2 || !cfg.IsDefaulted("cluster.systemNodes.minSize") {
		t.Errorf("an omitted minSize must default to 2 and be reported, got %d", cfg.Cluster.SystemNodes.MinSize)
	}
}

// Every value ApplyDefaults fills is reported with the value it used, and a value the file
// sets is not.
func TestLoad_ReportsEveryDefaultedValue(t *testing.T) {
	cfg, err := loadYAML(t, minimal(EnvStaging)+"  version: \"1.35\"\n")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, d := range cfg.Defaulted() {
		got[d.Field] = d.Value
	}
	for field, value := range map[string]string{
		"cloud.provider":                    "aws",
		"cluster.endpointPublicAccess":      "false",
		"cluster.systemNodes.instanceTypes": "[m7g.xlarge, m6g.xlarge]",
		"cluster.systemNodes.minSize":       "2",
		"cluster.network.vpcCidr":           defaultVPCCIDR,
		"observability.tier":                "full",
		"quotas.autoRequest":                "true",
		"quotas.vcpu":                       "256",
		"agentPlatform.enable":              "true",
		"agentPlatform.costPipeline":        "true",
		"agentPlatform.compliance.soc2":     "true",
		"org.gitops.eksGitopsRepo":          "github.com/acme/eks-gitops",
	} {
		if got[field] != value {
			t.Errorf("defaulted %s = %q, want %q (all: %v)", field, got[field], value, got)
		}
	}
	for _, set := range []string{"cluster.version", "cloud.region", "environment"} {
		if _, ok := got[set]; ok {
			t.Errorf("%s was not defaulted and must not be reported as defaulted", set)
		}
	}
}
