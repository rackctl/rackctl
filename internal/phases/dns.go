package phases

import (
	"encoding/json"
	"fmt"

	"github.com/rackctl/rackctl/internal/engine"
)

// dnsEnv builds the TF_VARs for landing-zone's dns component.
//
// Until this existed, `dns.hostedZone` was read by exactly one line of rackctl — the
// conditional in CoreComponents that decides whether to apply the component at all — and
// never reached the component itself. Every committed dns leaf pins a PLACEHOLDER domain in
// its own `inputs`, and those are what applied:
//
//	development  domain_name = "development.example.com", cert *.development.example.com
//	staging      domain_name = "staging.example.com",     cert *.staging.example.com
//	production   domain_name = "example.com",             cert *.example.com
//
// So `dns: {hostedZone: acme.example.com}` created a Route53 hosted zone for
// development.example.com, and published that string to
// /eks-agent-platform/<env>/dns/domain_filter — the parameter cluster-bootstrap reads to
// configure external-dns. external-dns then filtered on a domain the org does not own and
// managed no records at all, on a cluster whose every Application was Healthy.
//
// The expensive half is the certificate. dns/main.tf requests each acm_certificates entry
// with validation_method = "DNS" and, when wait_for_validation is true (its default),
// creates an aws_acm_certificate_validation that BLOCKS until ACM sees the validation
// record resolve publicly. Nobody owns example.com, so it never does. dns is applied by the
// SUBSTRATE phase — after the cluster phase has built a VPC and an EKS control plane — and
// clean-on-failure is on by default, so the operator paid for a cluster, waited out the
// validation timeout, and watched the whole thing be torn back down. Every `dns:`-enabled
// install ended this way, including one using the shipped examples/rackctl.yaml verbatim.
//
// Both variables have to be sent, and sending only domain_name would be worse than sending
// neither: the zone would then be correct while the certificate was still requested for
// *.development.example.com, so the validation records would be written into the org's real
// zone for a name it does not cover — the same unsatisfiable wait, now with a plausible-looking
// zone to make it harder to diagnose.
func dnsEnv(st *engine.State) ([]string, error) {
	// Unreachable by construction — CoreComponents appends "dns" only when the block is
	// present and non-empty — but this is the one place where getting it wrong reintroduces
	// exactly the bug above (an apply that silently uses the leaf's placeholder), so it fails
	// loudly instead of returning a partial environment.
	if st.Config.DNS == nil || st.Config.DNS.HostedZone == "" {
		return nil, fmt.Errorf("the dns component was scheduled with no dns.hostedZone to give it — " +
			"landing-zone's dns leaves pin a *.example.com placeholder in their own inputs, so applying " +
			"without an override would create a hosted zone for a domain the org does not own and block " +
			"on an ACM validation that can never succeed")
	}
	zone := st.Config.DNS.HostedZone

	env := []string{"TF_VAR_domain_name=" + zone}
	note(st, "dns: TF_VAR_domain_name=%s — the hosted zone and external-dns's domain filter. "+
		"Every committed dns leaf pins a *.example.com placeholder, so without this the zone and "+
		"the SSM domain_filter are both wrong", zone)

	// wait_for_validation is FALSE, and that is the load-bearing part rather than a
	// convenience. dns_mode is "create": this component is minting the zone right now, so at
	// apply time the registrar's NS records still point wherever they pointed before. ACM
	// validates by resolving the record over public DNS, which cannot succeed until a human
	// repoints the delegation — an out-of-band act that may take days. Waiting on it inside a
	// day-0 install makes the install fail for a reason that is not a failure.
	//
	// The certificate is still requested, and still validates on its own once delegation
	// lands: ACM retries for 72 hours, and the validation record is already in the zone. So
	// nothing is lost except the block.
	certs := map[string]any{
		"wildcard": map[string]any{
			"domain_name":               "*." + zone,
			"subject_alternative_names": []string{zone},
			"wait_for_validation":       false,
		},
	}
	// JSON, because acm_certificates is a map(object(...)) and TF_VAR_ carries complex types
	// as an HCL expression, which JSON object syntax satisfies. Verified against this repo's
	// OpenTofu with the component's exact variable type, including its optional() attributes,
	// and through terragrunt against a leaf that pins the placeholder: the ambient value
	// replaces the leaf's map WHOLESALE rather than deep-merging into it, so
	// wait_for_validation resolves to false rather than inheriting the leaf's default of true.
	// A deep merge would have left the block in place and the fix inert.
	blob, err := json.Marshal(certs)
	if err != nil {
		// A map of strings and bools cannot fail to marshal, so this is unreachable — but
		// returning env without the certificate override would silently leave the leaf's
		// placeholder in force, which is the failure this function exists to prevent.
		return nil, fmt.Errorf("marshalling the acm_certificates override for %s: %w", zone, err)
	}
	env = append(env, "TF_VAR_acm_certificates="+string(blob))

	note(st, "dns: TF_VAR_acm_certificates — one wildcard cert for *.%s (SAN %s), requested but "+
		"NOT waited on", zone, zone)
	note(st, "dns: point %s's NS records at this zone's name servers to finish issuing the "+
		"certificate. rackctl does not wait: the zone is being created by this apply, so the "+
		"delegation cannot exist yet and ACM's DNS validation could never resolve. It retries "+
		"for 72 hours and the validation record is already in the zone, so the cert issues on "+
		"its own once the delegation lands", zone)

	return env, nil
}
