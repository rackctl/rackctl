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
	env, err := dnsEnv(st, "apply")
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
	env, err := dnsEnv(st, "apply")
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

// A run that does not disclose the delegation leaves the operator with a zone, a pending
// certificate, and no idea what is expected of them — the request expires at 72 hours.
//
// What the note may NOT do is name a remedy. Whether the delegation is rackctl's to make or
// the operator's depends on whether the parent is a public hosted zone in this account, and
// that is not known until the zone exists — delegateSubdomain runs after this apply and
// reports which it was. The negative assertion is the load-bearing half: telling an operator
// rackctl cannot delegate sends them to hand-edit records rackctl has already written.
func TestDNSEnv_DisclosesTheDelegationClockWithoutGuessingTheRemedy(t *testing.T) {
	st, out := dnsState(t, "acme.example.com")
	if _, err := dnsEnv(st, "apply"); err != nil {
		t.Fatalf("dnsEnv: %v", err)
	}
	got := out.String()
	for _, want := range []string{"delegation", "72 hours"} {
		if !strings.Contains(got, want) {
			t.Fatalf("the note must name %q — without the delegation and the clock, an operator "+
				"cannot tell a slow certificate from a dead one.\ngot:\n%s", want, got)
		}
	}
	if strings.Contains(got, "rackctl cannot") {
		t.Fatalf("the note must not claim rackctl cannot delegate: when the parent is a public "+
			"zone in this account it does, automatically, moments later. Saying otherwise sends "+
			"the operator to edit records that are already correct.\ngot:\n%s", got)
	}
}

// componentEnv is what the apply and destroy paths both go through, so the wiring matters as
// much as the builder. A dns case that is never reached is the original bug.
func TestComponentEnv_WiresDNS(t *testing.T) {
	st, _ := dnsState(t, "acme.example.com")
	env, err := componentEnv(context.Background(), st, "dns", "apply")
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
		env, err := componentEnv(context.Background(), st, comp, "apply")
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

	if _, err := dnsEnv(st, "apply"); err == nil {
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

// The third leaf pin, and the one that made `environment: production` plus a dns block fail
// every time even after the domain and certificate were fixed.
//
// The production dns leaf pins enable_dnssec = true (development and staging pin false). Under
// create mode that mints an ECC_NIST_P256/SIGN_VERIFY KMS key and hands its ARN to
// aws_route53_key_signing_key — but Route53 requires a DNSSEC signing key in us-east-1, and the
// component declares no aliased provider, so the key lands in the leaf's own region. Every
// committed leaf is us-west-2 and componentDir can only address
// live/aws/workload-<env>/<region>/…, so there is no region where this succeeds.
// CreateKeySigningKey rejects it in the substrate phase, after the cluster is built, and
// clean-on-failure tears the whole run down — the same outcome as the placeholder domain, on a
// different pin.
func TestDNSEnv_TurnsOffTheDNSSECTheProductionLeafPins(t *testing.T) {
	st, _ := dnsState(t, "acme.example.com")
	env, err := dnsEnv(st, "apply")
	if err != nil {
		t.Fatalf("dnsEnv: %v", err)
	}
	if !contains(env, "TF_VAR_enable_dnssec=false") {
		t.Fatalf("the production leaf pins enable_dnssec = true, which needs a signing key in "+
			"us-east-1 while the component mints one in the leaf's region — so the apply dies at "+
			"CreateKeySigningKey after the cluster is already built.\ngot: %v", env)
	}
}

// The forward-looking instruction must not print during a teardown.
//
// componentEnv is shared by apply and destroy — deliberately, so the two cannot drift on which
// variables they inject — but a note saying "point your NS records at this zone's name servers
// to finish issuing the certificate … the zone is being created by this apply" is an imperative
// aimed at a human, and under `destroy dns` the run is DELETING that zone. An operator who acts
// on it repoints a live domain's delegation at name servers being torn down.
func TestDNSEnv_DoesNotTellTheOperatorToDelegateDuringATeardown(t *testing.T) {
	st, out := dnsState(t, "acme.example.com")
	if _, err := dnsEnv(st, "destroy"); err != nil {
		t.Fatalf("dnsEnv: %v", err)
	}
	if strings.Contains(out.String(), "NS records") {
		t.Fatalf("a destroy printed the NS-delegation instruction. The zone is being deleted, so "+
			"this tells the operator to point a live domain at name servers that are going "+
			"away.\ngot:\n%s", out.String())
	}
}

// But the variables themselves must still be injected on the destroy path — a destroy plan
// needs the same inputs the apply used, and a domain_name that reverted to the leaf's
// placeholder would have terraform planning against a zone rackctl never created.
func TestDNSEnv_StillInjectsEverythingOnDestroy(t *testing.T) {
	st, _ := dnsState(t, "acme.example.com")
	env, err := dnsEnv(st, "destroy")
	if err != nil {
		t.Fatalf("dnsEnv: %v", err)
	}
	for _, want := range []string{"TF_VAR_domain_name=acme.example.com", "TF_VAR_enable_dnssec=false"} {
		if !contains(env, want) {
			t.Errorf("destroy must inject %q too — suppressing a NOTE is not the same as dropping "+
				"the variable, and a destroy planned against the leaf's placeholder domain targets a "+
				"zone this platform never created", want)
		}
	}
}
