package phases

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

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

// delegateSubdomain writes the child zone's NS records into its parent, when the parent is
// a public hosted zone in this same account.
//
// The dns component mints a zone and requests a wildcard certificate for it, and ACM
// validates by resolving a record over PUBLIC DNS. Until the parent delegates, that
// resolution fails — the zone exists but nothing on the internet is pointed at it — and
// the request sits in PENDING_VALIDATION until it hard-expires at 72 hours.
//
// rackctl used to print "NEXT, and rackctl cannot do it" and leave the operator with a
// 72-hour clock. That is true for a zone whose parent is a registrar, and false for the
// common case this exists for: a platform installed on a subdomain of a domain the org
// already hosts in Route53. There the parent zone is one API call away, the delegation is
// a single additive RRSet, and the certificate issues within minutes instead of whenever
// somebody remembers.
//
// It is deliberately narrow. It writes exactly one NS RRSet for exactly the child zone,
// into a zone that must already exist in this account; it never creates the parent, never
// touches another record, and never runs when the parent is absent. A delegation is the
// one edit that is safe to make on someone's behalf, because it is inert until the child
// zone answers and the child zone is the thing this run just created.
func delegateSubdomain(ctx context.Context, st *engine.State) {
	if st.Config.DNS == nil || st.Config.DNS.HostedZone == "" {
		return
	}
	zone := strings.TrimSuffix(st.Config.DNS.HostedZone, ".")

	// The parent is the zone minus its first label. A two-label name (example.com) has no
	// parent zone anyone can hold — its delegation lives at the registrar — so there is
	// nothing to try.
	_, parent, found := strings.Cut(zone, ".")
	if !found || strings.Count(parent, ".") == 0 {
		note(st, "dns: %s is an apex domain, so its delegation is a registrar change. Point its NS "+
			"records at this zone's name servers; ACM cannot validate the certificate until you do, "+
			"and the request expires after 72 hours", zone)
		return
	}

	if st.Runner.DryRun {
		note(st, "dns: (apply) if %s is a public hosted zone in this account, rackctl writes %s's NS "+
			"records into it — the delegation ACM needs to validate the certificate. If it is not, "+
			"rackctl says so and the delegation stays a registrar change", parent, zone)
		return
	}

	childID := publicZoneID(ctx, st, zone)
	parentID := publicZoneID(ctx, st, parent)
	if childID == "" {
		note(st, "dns: could not find the hosted zone for %s to read its name servers — do the "+
			"delegation by hand", zone)
		return
	}
	if parentID == "" {
		note(st, "dns: %s is not a public hosted zone in this account, so the delegation is somebody "+
			"else's change. Point %s's NS records at this zone's name servers; ACM cannot validate "+
			"the certificate until that resolves publicly, and the request expires after 72 hours",
			parent, zone)
		return
	}

	out, err := st.Runner.Capture(ctx, "aws", "route53", "get-hosted-zone", "--id", childID,
		"--query", "DelegationSet.NameServers", "--output", "json")
	if err != nil {
		note(st, "dns: could not read %s's name servers (%v) — do the delegation by hand", zone, err)
		return
	}
	var servers []string
	if err := json.Unmarshal([]byte(out), &servers); err != nil || len(servers) == 0 {
		note(st, "dns: %s reported no name servers — do the delegation by hand", zone)
		return
	}

	records := make([]map[string]string, 0, len(servers))
	for _, s := range servers {
		records = append(records, map[string]string{"Value": s})
	}
	batch, err := json.Marshal(map[string]any{
		"Comment": "rackctl: delegate " + zone,
		"Changes": []any{map[string]any{
			// UPSERT, not CREATE: a re-run must be a no-op rather than a collision, and an
			// existing delegation pointing at a zone this run replaced must be corrected
			// rather than left stale.
			"Action": "UPSERT",
			"ResourceRecordSet": map[string]any{
				"Name": zone + ".", "Type": "NS", "TTL": 300, "ResourceRecords": records,
			},
		}},
	})
	if err != nil {
		note(st, "dns: could not build the delegation change for %s (%v) — do it by hand", zone, err)
		return
	}
	if err := st.Runner.Run(ctx, "aws", "route53", "change-resource-record-sets",
		"--hosted-zone-id", parentID, "--change-batch", string(batch)); err != nil {
		note(st, "dns: writing %s's delegation into %s failed (%v) — do it by hand", zone, parent, err)
		return
	}
	note(st, "dns: delegated %s from %s (%s). ACM resolves the validation record over public DNS, so "+
		"the wildcard certificate issues on its own from here", zone, parent, strings.Join(servers, ", "))
}

// publicZoneID resolves a zone name to its id, or "" when this account has no PUBLIC
// hosted zone by that exact name.
//
// The exact-name match and the private-zone exclusion are both load-bearing.
// list-hosted-zones-by-name is a PREFIX walk, so asking for nanohype.dev also returns
// dev.nanohype.dev and anything else sorting after it; and a private zone by the same name
// is a split-horizon view that resolves for the VPC and for nobody on the internet, which
// is precisely the audience ACM validates from.
func publicZoneID(ctx context.Context, st *engine.State, name string) string {
	out, err := st.Runner.Capture(ctx, "aws", "route53", "list-hosted-zones-by-name",
		"--dns-name", name+".",
		"--query", fmt.Sprintf("HostedZones[?Name=='%s.' && Config.PrivateZone==`false`].Id | [0]", name),
		"--output", "text")
	if err != nil {
		return ""
	}
	id := strings.TrimSpace(out)
	if id == "" || id == "None" {
		return ""
	}
	return id
}
