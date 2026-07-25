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
// Both variables have to be sent, and the certificate is not a nicety on top of the zone.
// dns/main.tf derives each validation record's NAME from the certificate and its ZONE from
// domain_name, so overriding only domain_name asks Route53 to write
// `_<hash>.development.example.com` into the acme.example.com zone. Route53 rejects an
// out-of-zone RRSet with InvalidChangeBatch, so that combination fails within seconds at
// record creation — loud and fast, but still after the cluster is built and therefore still
// a full teardown.
//
// That is the durable reason for the override, and it is worth stating precisely because it
// SURVIVES wait_for_validation = false: the validation record must fall inside the zone this
// component creates, whatever the wait is set to. A justification framed around the wait
// would read as obsolete the moment the wait was disabled, and invite someone to drop the
// override.
func dnsEnv(st *engine.State, verb string) ([]string, error) {
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
	// The certificate is still requested, and the validation record is already in the zone,
	// so it issues on its own IF the delegation lands inside ACM's window. That window is a
	// hard expiry, not a patient retry: after 72 hours the request reaches "Validation timed
	// out" and has to be re-requested. Since the delegation is a registrar change that may
	// well take longer than three days, the operator note below says so and says what to do
	// — a re-apply of the dns component requests a fresh certificate, because the provider
	// treats a timed-out certificate as absent and clears it from state.
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

	// enable_dnssec = false, for the same reason and against the same class of leaf pin.
	//
	// The PRODUCTION dns leaf pins enable_dnssec = true (development and staging pin false).
	// Under create mode that builds an aws_kms_key with ECC_NIST_P256/SIGN_VERIFY and hands
	// its ARN to aws_route53_key_signing_key — but Route53 requires a DNSSEC signing key in
	// us-east-1, and the dns component declares no aliased provider, so the key is minted in
	// the leaf's own region. Every committed leaf is us-west-2, and componentDir can only
	// address live/aws/workload-<env>/<region>/…, so there is no region where this succeeds.
	// CreateKeySigningKey rejects it immediately, in the substrate phase, after the cluster is
	// built — the same paid-for-then-torn-down outcome as the placeholder domain, on a
	// different pin. `environment: production` plus a dns block hit it every time.
	//
	// False is also the semantically right answer, not just the one that applies: DNSSEC
	// establishes a chain of trust from the parent zone, and the parent delegation has not
	// been repointed yet at day 0 — there is nothing to chain to. Signing a zone is an owner
	// act to take deliberately once delegation is live, which is why it is not something a
	// day-0 installer should be turning on implicitly.
	env = append(env, "TF_VAR_enable_dnssec=false")

	note(st, "dns: TF_VAR_acm_certificates — one wildcard cert for *.%s (SAN %s), requested but "+
		"NOT waited on", zone, zone)
	note(st, "dns: TF_VAR_enable_dnssec=false — the production leaf pins this true, which needs a "+
		"signing key in us-east-1 while the component mints it in the leaf's own region, so the "+
		"apply fails at CreateKeySigningKey. There is also nothing to chain trust to until the "+
		"parent delegation is repointed; sign the zone deliberately once it is")

	// Apply only. Told to an operator during `destroy dns`, this instructs them to point a
	// live domain's delegation at name servers the same run is in the middle of deleting.
	if verb != "destroy" {
		note(st, "dns: NEXT, and rackctl cannot do it — point %s's NS records at this zone's name "+
			"servers. Until that lands, ACM cannot resolve the certificate's validation record over "+
			"public DNS, and the request EXPIRES after 72 hours with status \"Validation timed out\". "+
			"If the delegation takes longer than that, re-apply the dns component to request a fresh "+
			"certificate. rackctl does not wait, because the zone is being created by this apply, so "+
			"the delegation cannot exist yet", zone)
	}

	return env, nil
}
