package phases

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/engine"
	"github.com/rackctl/rackctl/internal/exec"
)

func dnsState(t *testing.T, zone string) (*engine.State, *strings.Builder) {
	t.Helper()
	var out strings.Builder
	run := exec.New(&out)
	run.DryRun = true
	cfg := config.Default()
	cfg.DNS = &config.DNS{HostedZone: zone}
	return &engine.State{Config: cfg, Runner: run}, &out
}

// domain_name is the whole reason the component is applied, and rackctl never sent it. Every
// committed dns leaf pins a placeholder in its own inputs (development.example.com,
// staging.example.com, example.com), and an ambient TF_VAR is the only thing that beats a
// leaf's inputs — so without this the operator's zone is not created and
// /eks-agent-platform/<env>/dns/domain_filter carries a domain the org does not own, which
// external-dns then filters on and manages nothing.
func TestDNSEnv_SendsTheOperatorsZone(t *testing.T) {
	st, _ := dnsState(t, "acme.example.com")
	env, err := dnsEnv(st)
	if err != nil {
		t.Fatalf("dnsEnv: %v", err)
	}
	if !contains(env, "TF_VAR_domain_name=acme.example.com") {
		t.Fatalf("the operator's hosted zone must reach the dns component, or the leaf's "+
			"*.example.com placeholder applies instead.\ngot: %v", env)
	}
}

// The certificate is the expensive half, and sending domain_name ALONE would be worse than
// sending neither.
//
// dns/main.tf requests every acm_certificates entry with validation_method = "DNS" and, when
// wait_for_validation is true (its default), creates an aws_acm_certificate_validation that
// blocks until ACM resolves the record publicly. With only domain_name overridden, the zone
// is the org's real one while the cert is still requested for *.development.example.com — a
// name that zone does not cover — so the wait is unsatisfiable and now harder to diagnose.
//
// dns is applied by the SUBSTRATE phase, after the cluster phase has built a VPC and an EKS
// control plane, and clean-on-failure is on by default. So this is not a slow install: it is
// a paid-for cluster torn back down.
func TestDNSEnv_OverridesTheCertificateToo(t *testing.T) {
	st, _ := dnsState(t, "acme.example.com")
	env, err := dnsEnv(st)
	if err != nil {
		t.Fatalf("dnsEnv: %v", err)
	}

	var raw string
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "TF_VAR_acm_certificates="); ok {
			raw = v
		}
	}
	if raw == "" {
		t.Fatalf("acm_certificates must be overridden as well as domain_name — the leaf pins a "+
			"cert for *.development.example.com and waits on a DNS validation that can never "+
			"resolve.\ngot: %v", env)
	}

	var certs map[string]struct {
		DomainName string   `json:"domain_name"`
		SANs       []string `json:"subject_alternative_names"`
		Wait       *bool    `json:"wait_for_validation"`
	}
	if err := json.Unmarshal([]byte(raw), &certs); err != nil {
		t.Fatalf("acm_certificates is a map(object(...)); TF_VAR_ carries it as an HCL expression, "+
			"which JSON object syntax satisfies. This value is not valid JSON: %v\n%s", err, raw)
	}
	c, ok := certs["wildcard"]
	if !ok {
		t.Fatalf("expected a wildcard entry, matching the shape every committed leaf uses; got %s", raw)
	}
	if c.DomainName != "*.acme.example.com" {
		t.Errorf("cert domain = %q, want *.acme.example.com", c.DomainName)
	}
	if len(c.SANs) != 1 || c.SANs[0] != "acme.example.com" {
		t.Errorf("the apex must be a SAN so the cert covers the zone itself; got %v", c.SANs)
	}

	// The one assertion that cannot be relaxed. wait_for_validation must be present AND
	// false: the ambient map replaces the leaf's wholesale rather than deep-merging, so an
	// omitted field falls back to the VARIABLE's default, which is true — the blocking
	// behaviour this override exists to remove.
	if c.Wait == nil {
		t.Fatalf("wait_for_validation must be sent explicitly. The override replaces the leaf's "+
			"map wholesale, so omitting it inherits the variable default of TRUE and the install "+
			"blocks on an ACM validation that cannot complete.\ngot: %s", raw)
	}
	if *c.Wait {
		t.Fatalf("wait_for_validation must be false. dns_mode is create — this apply is MINTING "+
			"the zone, so the registrar's NS records still point elsewhere and ACM's public "+
			"resolution of the validation record cannot succeed until a human repoints the "+
			"delegation. ACM retries for 72 hours and the record is already in the zone, so "+
			"nothing is lost by not waiting.\ngot: %s", raw)
	}
}

// The delegation is a human act rackctl cannot perform, so a run that does not say so leaves
// the operator with a zone, a pending certificate, and no idea what is expected of them.
func TestDNSEnv_DisclosesTheDelegationStep(t *testing.T) {
	st, out := dnsState(t, "acme.example.com")
	if _, err := dnsEnv(st); err != nil {
		t.Fatalf("dnsEnv: %v", err)
	}
	if !strings.Contains(out.String(), "NS records") {
		t.Fatalf("the operator has to repoint NS records for the certificate to issue; a run that "+
			"does not say so hands them a permanently pending cert.\ngot:\n%s", out.String())
	}
}

// componentEnv is what the apply and destroy paths both go through, so the wiring matters as
// much as the builder. A dns case that is never reached is the original bug.
func TestComponentEnv_WiresDNS(t *testing.T) {
	st, _ := dnsState(t, "acme.example.com")
	env, err := componentEnv(context.Background(), st, "dns")
	if err != nil {
		t.Fatalf("componentEnv(dns): %v", err)
	}
	if !contains(env, "TF_VAR_domain_name=acme.example.com") {
		t.Fatalf("componentEnv must route the dns component through dnsEnv; got %v", env)
	}
}

// And the variables must NOT leak onto other components. domain_name is declared by dns and
// by nothing else rackctl applies, and an ambient TF_VAR beats a leaf's inputs — the exact
// mechanism that made TF_VAR_cluster_name override six downstream components.
func TestComponentEnv_DNSVarsDoNotLeak(t *testing.T) {
	st, _ := dnsState(t, "acme.example.com")
	for _, comp := range []string{"network", "secrets", "agent-iam", "cluster-addons", "cluster-bootstrap"} {
		env, err := componentEnv(context.Background(), st, comp)
		if err != nil {
			t.Fatalf("componentEnv(%s): %v", comp, err)
		}
		for _, e := range env {
			if strings.HasPrefix(e, "TF_VAR_domain_name=") || strings.HasPrefix(e, "TF_VAR_acm_certificates=") {
				t.Errorf("%s received %q — only the dns component declares these", comp, e)
			}
		}
	}
}

// Unreachable through CoreComponents, which gates on a non-empty hostedZone. It fails loudly
// anyway, because the alternative — returning a partial environment — silently reinstates the
// leaf's placeholder, and that failure costs a provision and a teardown.
func TestDNSEnv_RefusesToApplyWithNoZone(t *testing.T) {
	var out strings.Builder
	run := exec.New(&out)
	run.DryRun = true
	st := &engine.State{Config: config.Default(), Runner: run} // DNS is nil

	if _, err := dnsEnv(st); err == nil {
		t.Fatal("applying dns with no hostedZone must be an error, not an apply that quietly " +
			"creates a hosted zone for example.com")
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
