package reap

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A role carrying SOME tags but no Repository tag must be refused, and the refusal must
// name what it did find.
//
// This is the shared-account case the whole ownership model exists for: a role tagged by
// another estate's tooling looks managed, and a sweep that treats "tagged" as "ours" would
// force-delete it. Naming the tags it saw is what lets an operator tell a sibling estate's
// role from an untagged one of their own.
func TestProves_TaggedByAnotherEstateIsRefusedAndSaysWhatItFound(t *testing.T) {
	own := Owner{Org: "acme", Cluster: "development-platform"}

	ok, why := own.Proves(map[string]string{"Project": "landing-zone", "Environment": "development"})
	if ok {
		t.Fatal("a role with no Repository tag is not provably ours — deleting it would be a guess")
	}
	for _, want := range []string{"Project=landing-zone", "Environment=development", "none of which distinguish"} {
		if !strings.Contains(why, want) {
			t.Errorf("the refusal must name what it found so an operator can tell whose role it is.\ngot: %s", why)
		}
	}
}

// Three refusals that must not collapse into one another. An untagged role, a role tagged
// only by a neighbouring estate, and a role tagged for a DIFFERENT org each need their own
// sentence, because the operator's next move differs in each case.
func TestProves_EachRefusalKeepsItsOwnReason(t *testing.T) {
	own := Owner{Org: "acme", Cluster: "development-platform"}

	for _, tc := range []struct {
		name string
		tags map[string]string
		want string
	}{
		{"untagged", map[string]string{}, "carries no tags at all"},
		{"foreign org", map[string]string{"Repository": "other/eks-agent-platform"}, "is not under"},
		{"unrecognised tags only", map[string]string{"Name": "scratch"}, "no Repository tag and no ManagedBy"},
	} {
		ok, why := own.Proves(tc.tags)
		if ok {
			t.Errorf("%s: not provably ours", tc.name)
		}
		if !strings.Contains(why, tc.want) {
			t.Errorf("%s: refusal must say %q.\ngot: %s", tc.name, tc.want, why)
		}
	}
}

// The two proofs that DO establish ownership.
func TestProves_RepositoryUnderTheOrgOrTheOperatorMarker(t *testing.T) {
	own := Owner{Org: "acme", Cluster: "development-platform"}

	if ok, why := own.Proves(map[string]string{"Repository": "acme/eks-agent-platform"}); !ok {
		t.Errorf("a Repository under the org proves ownership: %s", why)
	}
	if ok, why := own.Proves(map[string]string{"ManagedBy": "eks-agent-platform"}); !ok {
		t.Errorf("the operator's own ManagedBy marker proves ownership: %s", why)
	}
}

// tagsRunner answers list-role-tags with a canned payload.
type tagsRunner struct {
	out     string
	err     error
	captErr error
}

func (r *tagsRunner) Query(_ context.Context, _ string, _ ...string) (string, error) {
	return r.out, r.err
}
func (r *tagsRunner) Capture(_ context.Context, _ string, _ ...string) (string, error) {
	return r.out, r.captErr
}
func (r *tagsRunner) Run(context.Context, string, ...string) error { return nil }

// Unparseable tag output must be an ERROR, not an empty map.
//
// The two must never collapse: an empty map means "provably untagged, therefore not ours"
// and leads to a refusal, while an unreadable response means the question was not answered
// — and treating the second as the first turns a read failure into a confident verdict.
func TestRoleTags_UnparseableIsAnErrorNotAnEmptyMap(t *testing.T) {
	tags, err := roleTags(context.Background(), &tagsRunner{out: "not json at all"}, "development-platform-app-tenant")
	if err == nil {
		t.Fatal("unparseable tag output must be an error — an empty map reads as 'provably untagged'")
	}
	if tags != nil {
		t.Errorf("no tag map may be returned alongside the error; got %v", tags)
	}
	if !strings.Contains(err.Error(), "development-platform-app-tenant") {
		t.Errorf("the error must name the role it could not read.\ngot: %v", err)
	}
}

func TestRoleTags_ParsesTheTagList(t *testing.T) {
	got, err := roleTags(context.Background(), &tagsRunner{
		out: `{"Tags": [{"Key": "Repository", "Value": "nanohype/eks-agent-platform"}, {"Key": "Component", "Value": "tenant-iam"}]}`,
	}, "development-platform-app-tenant")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["Repository"] != "nanohype/eks-agent-platform" || got["Component"] != "tenant-iam" {
		t.Fatalf("tags did not round-trip: %v", got)
	}
}

// roleQuery models the IAM calls forceDeleteRole makes, failing whichever one is named.
type roleQuery struct {
	attached, inline string
	failOn           string
	runs             []string
}

func (r *roleQuery) Query(ctx context.Context, n string, a ...string) (string, error) {
	return r.Capture(ctx, n, a...)
}
func (r *roleQuery) Capture(_ context.Context, _ string, args ...string) (string, error) {
	joined := strings.Join(args, " ")
	if strings.Contains(joined, r.failOn) && r.failOn != "" {
		return "", errors.New("NoSuchEntity")
	}
	switch {
	case strings.Contains(joined, "list-attached-role-policies"):
		return r.attached, nil
	case strings.Contains(joined, "list-role-policies"):
		return r.inline, nil
	}
	return "", nil
}
func (r *roleQuery) Run(_ context.Context, _ string, args ...string) error {
	joined := strings.Join(args, " ")
	r.runs = append(r.runs, joined)
	if r.failOn != "" && strings.Contains(joined, r.failOn) {
		return errors.New("DeleteConflict")
	}
	return nil
}

// A role cannot be deleted while anything is attached to it, so the order is the whole
// function: detach managed policies, delete inline policies, then delete the role.
func TestForceDeleteRole_DetachesBeforeDeleting(t *testing.T) {
	r := &roleQuery{attached: "arn:aws:iam::aws:policy/Boundary", inline: "tenant-baseline"}

	if err := forceDeleteRole(context.Background(), r, "development-platform-app-tenant"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"detach-role-policy", "delete-role-policy", "delete-role"}
	if len(r.runs) != len(want) {
		t.Fatalf("got %d calls, want %d: %v", len(r.runs), len(want), r.runs)
	}
	for i, w := range want {
		if !strings.Contains(r.runs[i], w) {
			t.Errorf("call %d = %q, want %q — a role cannot be deleted while a policy is attached",
				i, r.runs[i], w)
		}
	}
}

// Each step's failure must stop the sequence rather than press on to delete-role, which
// would fail on DeleteConflict and report a confusing reason for a knowable one.
func TestForceDeleteRole_StopsAtTheFirstFailedStep(t *testing.T) {
	for _, failOn := range []string{"list-attached-role-policies", "detach-role-policy", "list-role-policies", "delete-role-policy"} {
		r := &roleQuery{attached: "arn:aws:iam::aws:policy/Boundary", inline: "tenant-baseline", failOn: failOn}

		if err := forceDeleteRole(context.Background(), r, "development-platform-app-tenant"); err == nil {
			t.Errorf("a failure at %s must stop the sequence", failOn)
		}
		for _, ran := range r.runs {
			if strings.Contains(ran, "delete-role ") || strings.HasSuffix(ran, "delete-role") {
				t.Errorf("delete-role was attempted after %s failed; it can only fail on DeleteConflict", failOn)
			}
		}
	}
}

// A role with nothing attached deletes in one call.
func TestForceDeleteRole_BareRoleDeletesDirectly(t *testing.T) {
	r := &roleQuery{}

	if err := forceDeleteRole(context.Background(), r, "development-platform-app-tenant"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(r.runs) != 1 || !strings.Contains(r.runs[0], "delete-role") {
		t.Fatalf("a bare role needs exactly one call: %v", r.runs)
	}
}
