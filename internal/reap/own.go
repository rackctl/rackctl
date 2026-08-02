package reap

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Owner is the question every destructive enumeration in this package has to answer before
// it deletes anything: can this run PROVE it created the thing it is about to remove?
//
// In a dedicated account the question is free — everything in it belongs to the run, so a
// name filter is as good as a proof. rackctl's shipped topology is one account per startup
// (the maturity ladder's rung one), and an account that starts empty rarely stays that way:
// it accumulates other environments, then other projects, then things that have nothing to
// do with the platform at all.
//
// The deployment account this was written against holds three estates. Two of them tag with
// the IDENTICAL Project value, because both follow the same org tagging standard, which pins
// Project = "landing-zone" (nanohype/standards/resource-tagging.json). One of those two owns
// the account's CloudTrail bucket, its CUR bucket, and its inbound mail. So:
//
//	Project=landing-zone      does not discriminate — two estates share the value
//	ManagedBy=opentofu        does not discriminate — every estate here uses it
//	Environment=production    does not discriminate — every estate has one
//	Repository=<org>/<repo>   DOES discriminate, and is the only key that does
//
// Hence the rule this type enforces: a resource is ours when a tag SAYS so. An untagged
// match is not fair game — untagged means unknown, and unknown means refuse. Refusing costs
// a leftover resource and a loud message naming it. Guessing costs somebody's mail.
type Owner struct {
	// Org is config.Org.Name — the GitHub org whose repos this run deploys from. Repository
	// tags are "<org>/<repo>", so this is what makes the predicate belong to THIS run rather
	// than being a hardcoded list of nanohype's repo names.
	Org string
	// Cluster is config.ClusterName() — "<environment>-<cluster.name>". Used by callers for
	// name-shaped filters; Owner itself never assumes a resource carries it, because the
	// operator's role tags carry PlatformId, Tenant and Environment but no cluster key.
	Cluster string
}

// Proves reports whether these tags establish that the run may delete the resource, and when
// they do not, why — the reason is printed to the operator, so it names what was actually
// found rather than saying "not ours".
//
// Two independent proofs, either sufficient:
//
//   - Repository has the run's org prefix. This is the general one. It covers every resource
//     the terragrunt substrate stamps via live/root.hcl's provider default_tags, and every
//     Karpenter node, whose EC2NodeClass carries its own copy of the taxonomy.
//   - ManagedBy is exactly "eks-agent-platform". This is the operator's own marker
//     (platform_iam.go:182), and it is the channel the upstream IAM compromise runbook
//     already prescribes for finding operator-minted roles that a path prefix misses.
//
// What this deliberately does NOT prove is which CLUSTER a resource belongs to. Nothing the
// operator stamps carries a cluster key, so callers that need cluster scoping must still get
// it from a name or a cluster-exact tag; Proves is the second half of that pair, never a
// replacement for it.
func (o Owner) Proves(tags map[string]string) (bool, string) {
	if len(tags) == 0 {
		return false, "carries no tags at all — untagged means unknown, and unknown is treated as someone else's"
	}
	if repo, ok := tags["Repository"]; ok {
		if o.Org != "" && strings.HasPrefix(repo, o.Org+"/") {
			return true, ""
		}
		return false, fmt.Sprintf("Repository=%q is not under %q", repo, o.Org+"/")
	}
	if tags["ManagedBy"] == "eks-agent-platform" {
		return true, ""
	}
	// Naming the near-misses is the point. "Project=landing-zone" on a resource this run must
	// not touch is exactly the trap, so say that it was seen and that it was not enough.
	var seen []string
	for _, k := range []string{"Project", "ManagedBy", "Environment", "Component"} {
		if v, ok := tags[k]; ok {
			seen = append(seen, k+"="+v)
		}
	}
	if len(seen) > 0 {
		return false, "has no Repository tag; found only " + strings.Join(seen, ", ") +
			" — none of which distinguish this run's resources from another estate's in a shared account"
	}
	return false, "has no Repository tag and no ManagedBy=eks-agent-platform marker"
}

// roleTags reads an IAM role's tags. Read-only, so it runs in dry-run too — that is what
// lets a dry-run demonstrate the ownership decision rather than assert it.
//
// A role whose tags cannot be read returns an error rather than an empty map, because those
// two must not collapse: an empty map means "provably untagged, therefore not ours", and an
// unreadable role means "unknown", which has to refuse for a different reason and say so.
func roleTags(ctx context.Context, run execer, name string) (map[string]string, error) {
	out, err := run.Query(ctx, "aws", "iam", "list-role-tags", "--role-name", name, "--output", "json")
	if err != nil {
		return nil, err
	}
	var payload struct {
		Tags []struct{ Key, Value string } `json:"Tags"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		return nil, fmt.Errorf("could not parse tags for %s: %w", name, err)
	}
	tags := make(map[string]string, len(payload.Tags))
	for _, t := range payload.Tags {
		tags[t.Key] = t.Value
	}
	return tags, nil
}

// tagsFromDescribeJSON pulls the Tags array out of an ec2 describe-* element. EC2 spells the
// key "Tags" on most shapes and "TagSet" on network interfaces; both are handled so callers
// do not have to care which describe they used.
func tagsFromDescribeJSON(raw json.RawMessage) map[string]string {
	var shape struct {
		Tags   []struct{ Key, Value string } `json:"Tags"`
		TagSet []struct{ Key, Value string } `json:"TagSet"`
	}
	if err := json.Unmarshal(raw, &shape); err != nil {
		return nil
	}
	src := shape.Tags
	if len(src) == 0 {
		src = shape.TagSet
	}
	tags := make(map[string]string, len(src))
	for _, t := range src {
		tags[t.Key] = t.Value
	}
	return tags
}
