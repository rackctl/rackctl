package reap

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeEC2 answers the two enumerations the EC2 sweeps make and records every mutation.
//
// Both sweeps issue commands that destroy billable resources — terminate-instances and
// delete-volume — so what they SELECT is the whole safety property. A fake is the only way
// to assert the selection without a live account.
type fakeEC2 struct {
	instances  string // `--query Reservations[].Instances[].InstanceId --output text`
	volumes    string // `--query Volumes[].VolumeId --output text`
	unprovable string // volumes with CSI provenance and no cluster tag
	queryErr   error
	runErr     error
	runs       [][]string
}

func (f *fakeEC2) Query(_ context.Context, name string, args ...string) (string, error) {
	if f.queryErr != nil {
		return "", f.queryErr
	}
	joined := strings.Join(args, " ")
	switch {
	case strings.Contains(joined, "describe-instances"):
		return f.instances, nil
	case strings.Contains(joined, "describe-volumes") && strings.Contains(joined, "tag-key,Values=kubernetes.io/cluster/"):
		return f.volumes, nil
	case strings.Contains(joined, "describe-volumes"):
		return f.unprovable, nil
	}
	return "", nil
}

func (f *fakeEC2) Capture(ctx context.Context, name string, args ...string) (string, error) {
	return f.Query(ctx, name, args...)
}

func (f *fakeEC2) Run(_ context.Context, name string, args ...string) error {
	f.runs = append(f.runs, append([]string{name}, args...))
	return f.runErr
}

func (f *fakeEC2) ran(verb string) []string {
	for _, r := range f.runs {
		if len(r) > 1 && r[1] == "ec2" && len(r) > 2 && r[2] == verb {
			return r
		}
	}
	return nil
}

// A blank cluster name must select nothing. The filter is a tag value, so an empty one
// would widen `karpenter.sh/managed-by` to every instance in the region — an account-wide
// termination from a sweep whose whole contract is that it touches only this cluster's.
func TestOrphanedNodes_BlankClusterSelectsNothing(t *testing.T) {
	f := &fakeEC2{instances: "i-aaa i-bbb"}
	var out strings.Builder

	orphanedNodes(t.Context(), f, false, &out, "", "us-east-1")

	if len(f.runs) != 0 {
		t.Fatalf("a blank cluster name must terminate nothing; ran: %v", f.runs)
	}
}

// The sweep terminates exactly the instances the tag filter returned, in one call.
func TestOrphanedNodes_TerminatesTheTaggedInstances(t *testing.T) {
	f := &fakeEC2{instances: "i-aaa i-bbb"}
	var out strings.Builder

	orphanedNodes(t.Context(), f, false, &out, "development-platform", "us-east-1")

	got := f.ran("terminate-instances")
	if got == nil {
		t.Fatalf("nothing was terminated; a surviving Karpenter node holds the node security "+
			"group and stops the teardown on DependencyViolation. runs: %v", f.runs)
	}
	for _, want := range []string{"i-aaa", "i-bbb"} {
		if !strings.Contains(strings.Join(got, " "), want) {
			t.Errorf("%s was not terminated.\ngot: %v", want, got)
		}
	}
}

// A dry-run enumerates for real and shows the selection, but terminates nothing. That
// split is what makes `rackctl destroy --dry-run` a checkable claim rather than a restated
// filter.
func TestOrphanedNodes_DryRunShowsTheSelectionAndTerminatesNothing(t *testing.T) {
	f := &fakeEC2{instances: "i-aaa i-bbb"}
	var out strings.Builder

	orphanedNodes(t.Context(), f, true, &out, "development-platform", "us-east-1")

	if len(f.runs) != 0 {
		t.Fatalf("a dry-run must not terminate; ran: %v", f.runs)
	}
	for _, want := range []string{"i-aaa", "i-bbb"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the dry-run must name what it would terminate, or it demonstrates "+
				"nothing.\ngot: %s", out.String())
		}
	}
}

// An enumeration that FAILED is not an empty selection, and must not be reported as one.
// A silent skip here leaves a running instance holding the node security group, and the
// teardown then dies on DependencyViolation with the cluster already gone.
func TestOrphanedNodes_UnreadableEnumerationSaysTheSweepDidNotRun(t *testing.T) {
	f := &fakeEC2{queryErr: errors.New("AccessDeniedException")}
	var out strings.Builder

	orphanedNodes(t.Context(), f, false, &out, "development-platform", "us-east-1")

	if len(f.runs) != 0 {
		t.Fatalf("a failed enumeration must not be treated as a selection; ran: %v", f.runs)
	}
	if !strings.Contains(out.String(), "did NOT run") {
		t.Errorf("the operator must be told the backstop was skipped.\ngot: %s", out.String())
	}
}

// "None" is what the AWS CLI prints for an empty text-output query. Treating it as an
// instance id would send `terminate-instances --instance-ids None`.
func TestOrphanedNodes_EmptySelectionTerminatesNothing(t *testing.T) {
	for _, empty := range []string{"", "None", "  "} {
		f := &fakeEC2{instances: empty}
		var out strings.Builder

		orphanedNodes(t.Context(), f, false, &out, "development-platform", "us-east-1")

		if len(f.runs) != 0 {
			t.Fatalf("%q must select nothing; ran: %v", empty, f.runs)
		}
	}
}

// The volume sweep is the same contract on a resource that is deleted one at a time.
func TestOrphanedVolumes_BlankClusterSelectsNothing(t *testing.T) {
	f := &fakeEC2{volumes: "vol-aaa"}
	var out strings.Builder

	orphanedVolumes(t.Context(), f, false, &out, "", "us-east-1")

	if len(f.runs) != 0 {
		t.Fatalf("a blank cluster name must delete nothing; ran: %v", f.runs)
	}
}

func TestOrphanedVolumes_DeletesTheTaggedVolumes(t *testing.T) {
	f := &fakeEC2{volumes: "vol-aaa vol-bbb"}
	var out strings.Builder

	orphanedVolumes(t.Context(), f, false, &out, "development-platform", "us-east-1")

	var deleted []string
	for _, r := range f.runs {
		joined := strings.Join(r, " ")
		if strings.Contains(joined, "delete-volume") {
			deleted = append(deleted, joined)
		}
	}
	if len(deleted) != 2 {
		t.Fatalf("both tagged volumes must be deleted; got %d: %v", len(deleted), f.runs)
	}
}

func TestOrphanedVolumes_DryRunDeletesNothing(t *testing.T) {
	f := &fakeEC2{volumes: "vol-aaa"}
	var out strings.Builder

	orphanedVolumes(t.Context(), f, true, &out, "development-platform", "us-east-1")

	if len(f.runs) != 0 {
		t.Fatalf("a dry-run must not delete; ran: %v", f.runs)
	}
	if !strings.Contains(out.String(), "vol-aaa") {
		t.Errorf("the dry-run must name what it would delete.\ngot: %s", out.String())
	}
}

func TestOrphanedVolumes_UnreadableEnumerationDeletesNothing(t *testing.T) {
	f := &fakeEC2{queryErr: errors.New("ThrottlingException")}
	var out strings.Builder

	orphanedVolumes(t.Context(), f, false, &out, "development-platform", "us-east-1")

	if len(f.runs) != 0 {
		t.Fatalf("a failed enumeration must not be treated as a selection; ran: %v", f.runs)
	}
	if !strings.Contains(out.String(), "did NOT run") {
		t.Errorf("the operator must be told the sweep was skipped.\ngot: %s", out.String())
	}
}

// A dry-run that selects nothing must SAY it selected nothing. That sentence is the whole
// value of enumerating for real: in an account holding more than one estate, "this sweep
// is correctly scoped" is a claim worth demonstrating rather than asserting, and a
// dry-run that printed nothing could only ever restate its own filter back.
func TestOrphanedNodes_DryRunSaysWhenItSelectsNothing(t *testing.T) {
	for _, empty := range []string{"", "None"} {
		f := &fakeEC2{instances: empty}
		var out strings.Builder

		orphanedNodes(t.Context(), f, true, &out, "development-platform", "us-east-1")

		if !strings.Contains(out.String(), "selects nothing") {
			t.Errorf("an empty selection must be stated, not silent.\ngot: %s", out.String())
		}
	}
}

// A failed termination is the case that wedges the teardown: the instance keeps the node
// security group, terraform cannot delete a security group in use, and the destroy stops
// with the cluster already gone. The operator has to be told which failure they are in.
func TestOrphanedNodes_FailedTerminationNamesTheConsequence(t *testing.T) {
	f := &fakeEC2{instances: "i-aaa", runErr: errors.New("UnauthorizedOperation")}
	var out strings.Builder

	orphanedNodes(t.Context(), f, false, &out, "development-platform", "us-east-1")

	got := out.String()
	if !strings.Contains(got, "DependencyViolation") {
		t.Errorf("a failed termination must name the teardown failure it causes.\ngot: %s", got)
	}
	if !strings.Contains(got, "UnauthorizedOperation") {
		t.Errorf("the tool's own reason must survive into the message.\ngot: %s", got)
	}
}

func TestOrphanedVolumes_DryRunSaysWhenItSelectsNothing(t *testing.T) {
	for _, empty := range []string{"", "None"} {
		f := &fakeEC2{volumes: empty}
		var out strings.Builder

		orphanedVolumes(t.Context(), f, true, &out, "development-platform", "us-east-1")

		if !strings.Contains(out.String(), "selects nothing") {
			t.Errorf("an empty selection must be stated, not silent.\ngot: %s", out.String())
		}
	}
}

// One volume failing to delete must not abandon the rest: they each bill independently,
// and a sweep that stops at the first error leaves the others paying.
func TestOrphanedVolumes_OneFailureDoesNotAbandonTheRest(t *testing.T) {
	f := &fakeEC2{volumes: "vol-aaa vol-bbb", runErr: errors.New("VolumeInUse")}
	var out strings.Builder

	orphanedVolumes(t.Context(), f, false, &out, "development-platform", "us-east-1")

	var attempts int
	for _, r := range f.runs {
		if strings.Contains(strings.Join(r, " "), "delete-volume") {
			attempts++
		}
	}
	if attempts != 2 {
		t.Fatalf("both volumes must be attempted; got %d — a sweep that stops at the first "+
			"error leaves the rest billing", attempts)
	}
	if !strings.Contains(out.String(), "keep billing") {
		t.Errorf("a failed delete must name the cost.\ngot: %s", out.String())
	}
}

// A volume with EBS CSI provenance and no cluster tag is provably SOME cluster's and might
// be a sibling environment's in the same account. It is named and left, never deleted:
// deleting it would be a guess, and an operator told "three volumes are probably yours and
// I will not guess" can act, while one told nothing pays indefinitely.
func TestOrphanedVolumes_UnprovableVolumesAreReportedNotDeleted(t *testing.T) {
	f := &fakeEC2{volumes: "", unprovable: `[
	  {"VolumeId": "vol-ccc", "Size": 100, "Tags": [{"Key": "CSIVolumeName", "Value": "pvc-123"}]},
	  {"VolumeId": "vol-ddd", "Size": 20, "Tags": [
	    {"Key": "CSIVolumeName", "Value": "pvc-456"},
	    {"Key": "kubernetes.io/cluster/development-platform", "Value": "owned"}]}
	]`}
	var out strings.Builder

	orphanedVolumes(t.Context(), f, false, &out, "development-platform", "us-east-1")

	for _, r := range f.runs {
		if strings.Contains(strings.Join(r, " "), "delete-volume") {
			t.Fatalf("an unprovable volume must never be deleted; ran: %v", f.runs)
		}
	}
	if !strings.Contains(out.String(), "vol-ccc") {
		t.Errorf("an unprovable volume must be NAMED so the operator can decide.\ngot: %s", out.String())
	}
}

// A volume that IS cluster-tagged is provable, so it must not appear in the unprovable
// report — it is the tagged sweep's business, and naming it in both places would tell the
// operator to hand-delete something rackctl already deleted.
func TestReportUnprovableVolumes_ClusterTaggedVolumesAreNotReported(t *testing.T) {
	f := &fakeEC2{unprovable: `[
	  {"VolumeId": "vol-tagged", "Size": 50, "Tags": [
	    {"Key": "CSIVolumeName", "Value": "pvc-1"},
	    {"Key": "kubernetes.io/cluster/development-platform", "Value": "owned"}]}
	]`}
	var out strings.Builder

	reportUnprovableVolumes(t.Context(), f, &out, "development-platform", "us-east-1")

	if strings.Contains(out.String(), "vol-tagged") {
		t.Errorf("a cluster-tagged volume is provable and is handled by the tagged sweep.\ngot: %s", out.String())
	}
}

// A volume with no CSI provenance at all is somebody else's entirely — an operator's own
// snapshot restore, another team's database. It must not be named.
func TestReportUnprovableVolumes_NonCSIVolumesAreNotReported(t *testing.T) {
	f := &fakeEC2{unprovable: `[{"VolumeId": "vol-manual", "Size": 8, "Tags": [{"Key": "Name", "Value": "scratch"}]}]`}
	var out strings.Builder

	reportUnprovableVolumes(t.Context(), f, &out, "development-platform", "us-east-1")

	if strings.Contains(out.String(), "vol-manual") {
		t.Errorf("a volume with no EBS CSI provenance is not this cluster's business.\ngot: %s", out.String())
	}
}

// Unreadable or unparseable output reports nothing rather than guessing.
func TestReportUnprovableVolumes_UnreadableReportsNothing(t *testing.T) {
	for _, raw := range []string{"", "not json"} {
		f := &fakeEC2{unprovable: raw}
		var out strings.Builder

		reportUnprovableVolumes(t.Context(), f, &out, "development-platform", "us-east-1")

		if out.String() != "" {
			t.Errorf("input %q must produce no claim.\ngot: %s", raw, out.String())
		}
	}
	f := &fakeEC2{queryErr: errors.New("AccessDenied")}
	var out strings.Builder
	reportUnprovableVolumes(t.Context(), f, &out, "development-platform", "us-east-1")
	if out.String() != "" {
		t.Errorf("a failed read must produce no claim.\ngot: %s", out.String())
	}
}

// The report names the volume, its size, and the durable upstream fix — an operator given
// only a count cannot act on it.
func TestReportUnprovableVolumes_NamesTheVolumeAndTheDurableFix(t *testing.T) {
	f := &fakeEC2{unprovable: `[{"VolumeId": "vol-ccc", "Size": 100, "Tags": [{"Key": "CSIVolumeName", "Value": "pvc-1"}]}]`}
	var out strings.Builder

	reportUnprovableVolumes(t.Context(), f, &out, "development-platform", "us-east-1")

	got := out.String()
	for _, want := range []string{"vol-ccc", "100GiB", "extraVolumeTags"} {
		if !strings.Contains(got, want) {
			t.Errorf("the report must carry %q so the operator can act.\ngot: %s", want, got)
		}
	}
}
