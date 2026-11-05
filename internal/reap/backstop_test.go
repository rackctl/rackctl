package reap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/exec"
)

// Each backstop reports what it did, so the end of a teardown can say which guards held
// rather than printing "platform destroyed" over a sweep that never ran.

func TestSummarize_NamesEveryBackstopAndSaysWhetherItWasClean(t *testing.T) {
	var out bytes.Buffer
	clean := Summarize(&out, []Backstop{
		{Name: "Karpenter nodes", Outcome: FoundNothing},
		{Name: "operator IAM roles", Outcome: Acted, Detail: "deleted 2"},
		{Name: "orphaned EBS volumes", Outcome: Failed, Detail: "could not delete vol-1"},
		{Name: "in-cluster reap", Outcome: NotRun, Detail: "API unreachable"},
		{Name: "unprovable EBS volumes", Outcome: LeftBehind, Informational: true, Detail: "vol-2"},
	})
	if clean {
		t.Error("a failed and an unrun backstop are not a clean summary")
	}
	for _, want := range []string{
		"backstop summary",
		"Karpenter nodes: found nothing",
		"operator IAM roles: ran — deleted 2",
		"orphaned EBS volumes: FAILED — could not delete vol-1",
		"in-cluster reap: did NOT run — API unreachable",
		"unprovable EBS volumes: left resources it could not prove were this run's — vol-2",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("summary is missing %q:\n%s", want, out.String())
		}
	}
}

func TestClean_InformationalBackstopsDoNotDecideIt(t *testing.T) {
	if !Clean([]Backstop{{Outcome: Acted}, {Outcome: NotRun, Informational: true}, {Outcome: LeftBehind}}) {
		t.Error("an informational report that did not run, and a sweep that left unprovable resources, " +
			"are not failures of a guard")
	}
	for _, o := range []Outcome{Failed, NotRun} {
		if Clean([]Backstop{{Outcome: FoundNothing}, {Outcome: o}}) {
			t.Errorf("a guard that %s is not clean", o)
		}
	}
	var out bytes.Buffer
	if !Summarize(&out, nil) || out.Len() != 0 {
		t.Errorf("no backstops prints nothing and is clean; got %q", out.String())
	}
}

// ─────────────────────────── unreachable ───────────────────────────

// fakeEKS answers describe-cluster, the one read unreachable makes.
type fakeEKS struct {
	answer string
	err    error
	args   []string
}

func (f *fakeEKS) Query(_ context.Context, _ string, args ...string) (string, error) {
	f.args = args
	return f.answer, f.err
}
func (f *fakeEKS) Capture(ctx context.Context, n string, a ...string) (string, error) {
	return f.Query(ctx, n, a...)
}
func (f *fakeEKS) Run(context.Context, string, ...string) error { return nil }

func withEgress(t *testing.T, ip string, err error) {
	t.Helper()
	prev := EgressIP
	EgressIP = func(context.Context) (string, error) { return ip, err }
	t.Cleanup(func() { EgressIP = prev })
}

// After a good repoint a readyz failure is a cluster that exists and does not answer from
// here. "No reachable cluster — nothing to reap" said the opposite, and the teardown then
// destroyed the cluster on top of what the reap exists to remove first.
func TestUnreachable_ALiveClusterIsAReapThatDidNotRun(t *testing.T) {
	withEgress(t, "198.51.100.7", nil)
	f := &fakeEKS{answer: `{"status":"ACTIVE","public":true,"private":true,"cidrs":["203.0.113.4/32"]}`}
	var out bytes.Buffer

	b := unreachable(context.Background(), f, &out, "staging-platform", "us-west-2", errors.New("i/o timeout"))

	if b.Outcome != NotRun {
		t.Fatalf("an unanswering live cluster is a reap that did not run, got %s", b.Outcome)
	}
	for _, want := range []string{"in-cluster reap did NOT run", "staging-platform exists (ACTIVE)", "i/o timeout",
		"public=true private=true", "203.0.113.4/32", "198.51.100.7 is NOT in it", "were not reaped"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report must carry %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "nothing to reap") {
		t.Errorf("a live cluster was reported as nothing to reap:\n%s", out.String())
	}
	if flag(f.args, "--name") != "staging-platform" || flag(f.args, "--region") != "us-west-2" {
		t.Errorf("describe-cluster must name the cluster and region, got %v", f.args)
	}
}

func TestUnreachable_EgressInsideTheAllowListPointsElsewhere(t *testing.T) {
	withEgress(t, "203.0.113.4", nil)
	f := &fakeEKS{answer: `{"status":"ACTIVE","public":true,"private":false,"cidrs":["203.0.113.0/24"]}`}
	var out bytes.Buffer
	unreachable(context.Background(), f, &out, "c", "r", errors.New("x"))
	if !strings.Contains(out.String(), "is inside it") {
		t.Errorf("an admitted egress must say the block is elsewhere:\n%s", out.String())
	}
}

func TestUnreachable_EgressDetectionFailureIsSaid(t *testing.T) {
	withEgress(t, "", errors.New("no route"))
	f := &fakeEKS{answer: `{"status":"ACTIVE","public":true,"cidrs":["203.0.113.0/24"]}`}
	var out bytes.Buffer
	unreachable(context.Background(), f, &out, "c", "r", errors.New("x"))
	if !strings.Contains(out.String(), "could not be detected (no route)") {
		t.Errorf("a failed egress lookup must be said:\n%s", out.String())
	}
}

func TestUnreachable_PrivateEndpointNeedsNoEgress(t *testing.T) {
	prev := EgressIP
	EgressIP = nil
	t.Cleanup(func() { EgressIP = prev })
	f := &fakeEKS{answer: `{"status":"ACTIVE","public":false,"private":true,"cidrs":null}`}
	var out bytes.Buffer
	b := unreachable(context.Background(), f, &out, "c", "r", errors.New("x"))
	if b.Outcome != NotRun || !strings.Contains(out.String(), "public=false private=true") {
		t.Errorf("a private endpoint is a reap that did not run, said with its posture: %s\n%s", b.Outcome, out.String())
	}
}

// A cluster EKS says is gone really is nothing to reap.
func TestUnreachable_AGoneClusterIsNothingToReap(t *testing.T) {
	f := &fakeEKS{err: errors.New("aws: exit status 254: An error occurred (ResourceNotFoundException)")}
	var out bytes.Buffer
	if b := unreachable(context.Background(), f, &out, "c", "r", errors.New("x")); b.Outcome != FoundNothing {
		t.Fatalf("a cluster EKS does not have is nothing to reap, got %s", b.Outcome)
	}
}

// When neither the API nor EKS answers, whether there is anything to reap is unknown.
func TestUnreachable_UnansweredDescribeIsNotRun(t *testing.T) {
	for name, f := range map[string]*fakeEKS{
		"describe fails": {err: errors.New("AccessDenied")},
		"unreadable":     {answer: "not json"},
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			if b := unreachable(context.Background(), f, &out, "c", "r", errors.New("x")); b.Outcome != NotRun {
				t.Fatalf("got %s", b.Outcome)
			}
		})
	}
	var out bytes.Buffer
	if b := unreachable(context.Background(), &fakeEKS{}, &out, "", "r", errors.New("x")); b.Outcome != NotRun {
		t.Fatalf("no cluster name is a reap that did not run, got %s", b.Outcome)
	}
}

// All, through the production Runner: a readyz failure reaches EKS rather than reporting
// nothing to reap.
func TestAll_AnUnansweringAPIIsReportedNotRun(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"kubectl": "#!/bin/sh\necho 'Unable to connect to the server: dial tcp: i/o timeout' >&2\nexit 1\n",
		"aws":     "#!/bin/sh\necho '{\"status\":\"ACTIVE\",\"public\":false,\"private\":true}'\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var out bytes.Buffer
	run := exec.New(&out)
	run.ReadAttempts = 1

	b := All(context.Background(), run, &out, "development-platform", "us-west-2")
	if b.Outcome != NotRun || !strings.Contains(out.String(), "in-cluster reap did NOT run") {
		t.Fatalf("got %s:\n%s", b.Outcome, out.String())
	}
}

// A reap whose deletes fail reports it failed, naming what.
func TestAll_FailedDeletesAreReported(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
case "$*" in
  *"get applications -o jsonpath"*) ;;
  *"get applications -o json"*) echo '{"items":[{"metadata":{"name":"a"},"spec":{"syncPolicy":{"automated":{}}}}]}' ;;
  *"delete platforms"*) exit 1 ;;
  *"delete pvc"*) exit 1 ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	run := exec.New(&bytes.Buffer{})
	run.ReadAttempts = 1

	b := All(context.Background(), run, &bytes.Buffer{}, "c", "r")
	if b.Outcome != Failed {
		t.Fatalf("got %s: %s", b.Outcome, b.Detail)
	}
	for _, want := range []string{"still armed", "platforms.platform.nanohype.dev did not finalize", "PVCs did not delete"} {
		if !strings.Contains(b.Detail, want) {
			t.Errorf("detail is missing %q: %s", want, b.Detail)
		}
	}

	// The PVC delete succeeding leaves the earlier failures standing.
	script = strings.Replace(script, `  *"delete pvc"*) exit 1 ;;`+"\n", "", 1)
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if b := All(context.Background(), run, &bytes.Buffer{}, "c", "r"); b.Outcome != Failed {
		t.Fatalf("a CR that did not finalize is a failed reap even when the PVCs went: %s", b.Outcome)
	}
}

func TestAll_DryRunDescribes(t *testing.T) {
	run := exec.New(&bytes.Buffer{})
	run.DryRun = true
	if b := All(context.Background(), run, &bytes.Buffer{}, "c", "r"); b.Outcome != Acted || !strings.Contains(b.Detail, "dry-run") {
		t.Fatalf("got %s: %s", b.Outcome, b.Detail)
	}
}

// ─────────────────────────── each sweep's outcome ───────────────────────────

func TestOperatorRoles_ReportsWhatItDid(t *testing.T) {
	own := Owner{Org: "nanohype", Cluster: "dev"}
	ctx := context.Background()

	if b := reapOperatorRoles(ctx, newFakeIAM(), false, &bytes.Buffer{}, Owner{Org: "nanohype"}); b.Outcome != NotRun {
		t.Errorf("no cluster to scope by: got %s", b.Outcome)
	}
	if b := reapOperatorRoles(ctx, newFakeIAM(), false, &bytes.Buffer{}, own); b.Outcome != FoundNothing {
		t.Errorf("no roles: got %s", b.Outcome)
	}

	f := newFakeIAM()
	f.attached["dev-a-tenant"] = []string{"arn:aws:iam::1:policy/x"}
	if b := reapOperatorRoles(ctx, f, false, &bytes.Buffer{}, own); b.Outcome != Acted || b.Detail != "deleted 1" {
		t.Errorf("one role deleted: got %s %q", b.Outcome, b.Detail)
	}

	f = newFakeIAM()
	f.attached["dev-a-tenant"] = []string{"arn:aws:iam::1:policy/x"}
	if b := reapOperatorRoles(ctx, f, true, &bytes.Buffer{}, own); b.Outcome != Acted || !strings.Contains(b.Detail, "dry-run") {
		t.Errorf("dry-run: got %s %q", b.Outcome, b.Detail)
	}

	f = newFakeIAM()
	f.attached["dev-a-tenant"] = []string{"arn:aws:iam::1:policy/x"}
	f.detachFails["dev-a-tenant"] = true
	if b := reapOperatorRoles(ctx, f, false, &bytes.Buffer{}, own); b.Outcome != Failed || !strings.Contains(b.Detail, "dev-a-tenant") {
		t.Errorf("a failed delete: got %s %q", b.Outcome, b.Detail)
	}

	for _, dry := range []bool{false, true} {
		f = newFakeIAM()
		f.attached["dev-a-tenant"] = []string{"arn:aws:iam::1:policy/x"}
		f.attached["dev-b-tenant"] = []string{"arn:aws:iam::1:policy/x"}
		f.tags["dev-b-tenant"] = map[string]string{"Project": "landing-zone"}
		if b := reapOperatorRoles(ctx, f, dry, &bytes.Buffer{}, own); b.Outcome != LeftBehind || !strings.Contains(b.Detail, "refused 1") {
			t.Errorf("dry=%v: a refused role is left behind, named: got %s %q", dry, b.Outcome, b.Detail)
		}
	}

	f = newFakeIAM()
	f.attached["dev-b-tenant"] = []string{"arn:aws:iam::1:policy/x"}
	f.tagsFail["dev-b-tenant"] = true
	if b := reapOperatorRoles(ctx, f, false, &bytes.Buffer{}, own); b.Outcome != LeftBehind {
		t.Errorf("only refused roles: got %s", b.Outcome)
	}
}

func TestOperatorRoles_UnenumerableIsNotRun(t *testing.T) {
	f := &fakeEKS{err: errors.New("AccessDenied")}
	if b := reapOperatorRoles(context.Background(), f, false, &bytes.Buffer{}, Owner{Org: "o", Cluster: "dev"}); b.Outcome != NotRun {
		t.Errorf("got %s", b.Outcome)
	}
}

func TestUnstickTerminating_ReportsWhatItDid(t *testing.T) {
	ctx := context.Background()
	if b := unstickTerminating(ctx, &fakeKube{}, true, &bytes.Buffer{}); b.Outcome != Acted {
		t.Errorf("dry-run: got %s", b.Outcome)
	}
	if b := unstickTerminating(ctx, &fakeKube{ready: false}, false, &bytes.Buffer{}); b.Outcome != NotRun {
		t.Errorf("unreachable is not nothing pinned: got %s", b.Outcome)
	}
	k := &fakeKube{ready: true, crds: map[string]bool{"platforms.platform.nanohype.dev": true},
		listing: map[string]string{"platforms.platform.nanohype.dev": "ns ops\n"}}
	if b := unstickTerminating(ctx, k, false, &bytes.Buffer{}); b.Outcome != Acted || b.Detail != "unstuck 1" {
		t.Errorf("one unstuck: got %s %q", b.Outcome, b.Detail)
	}
	k = &fakeKube{ready: true, crds: map[string]bool{"platforms.platform.nanohype.dev": true}}
	if b := unstickTerminating(ctx, k, false, &bytes.Buffer{}); b.Outcome != FoundNothing {
		t.Errorf("nothing pinned: got %s", b.Outcome)
	}
	fk := &failingKube{fakeKube: fakeKube{ready: true, crds: map[string]bool{"platforms.platform.nanohype.dev": true},
		listing: map[string]string{"platforms.platform.nanohype.dev": "ns ops\n"}}}
	if b := unstickTerminating(ctx, fk, false, &bytes.Buffer{}); b.Outcome != Failed {
		t.Errorf("a failed patch: got %s", b.Outcome)
	}
	fk = &failingKube{listFails: true, fakeKube: fakeKube{ready: true, crds: map[string]bool{"platforms.platform.nanohype.dev": true}}}
	if b := unstickTerminating(ctx, fk, false, &bytes.Buffer{}); b.Outcome != NotRun || !strings.Contains(b.Detail, "could not list") {
		t.Errorf("an unlistable kind: got %s %q", b.Outcome, b.Detail)
	}
}

// failingKube is fakeKube whose patches fail, or whose listings fail.
type failingKube struct {
	fakeKube
	listFails bool
}

func (k *failingKube) Capture(ctx context.Context, name string, args ...string) (string, error) {
	if k.listFails && len(args) >= 2 && args[0] == "get" && args[1] != "--raw" && args[1] != "crd" {
		return "", fmt.Errorf("Forbidden")
	}
	return k.fakeKube.Capture(ctx, name, args...)
}
func (k *failingKube) Query(ctx context.Context, name string, args ...string) (string, error) {
	return k.Capture(ctx, name, args...)
}
func (k *failingKube) Run(context.Context, string, ...string) error { return fmt.Errorf("conflict") }

func TestOrphanedNodes_ReportsWhatItDid(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		f    *fakeEC2
		dry  bool
		cl   string
		want Outcome
	}{
		{"no cluster", &fakeEC2{}, false, "", NotRun},
		{"unenumerable", &fakeEC2{queryErr: errors.New("AccessDenied")}, false, "c", NotRun},
		{"none", &fakeEC2{instances: "None"}, false, "c", FoundNothing},
		{"dry-run", &fakeEC2{instances: "i-1"}, true, "c", Acted},
		{"terminated", &fakeEC2{instances: "i-1 i-2"}, false, "c", Acted},
		{"terminate fails", &fakeEC2{instances: "i-1", runErr: errors.New("x")}, false, "c", Failed},
	} {
		if b := orphanedNodes(ctx, tc.f, tc.dry, &bytes.Buffer{}, tc.cl, "r"); b.Outcome != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, b.Outcome, tc.want)
		}
	}
}

func TestOrphanedVolumes_ReportsWhatItDid(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		f    *fakeEC2
		dry  bool
		cl   string
		want Outcome
	}{
		{"no cluster", &fakeEC2{}, false, "", NotRun},
		{"unenumerable", &fakeEC2{queryErr: errors.New("AccessDenied")}, false, "c", NotRun},
		{"none", &fakeEC2{volumes: ""}, false, "c", FoundNothing},
		{"dry-run", &fakeEC2{volumes: "vol-1"}, true, "c", Acted},
		{"deleted", &fakeEC2{volumes: "vol-1"}, false, "c", Acted},
		{"delete fails", &fakeEC2{volumes: "vol-1", runErr: errors.New("x")}, false, "c", Failed},
	} {
		bs := orphanedVolumes(ctx, tc.f, tc.dry, &bytes.Buffer{}, tc.cl, "r")
		if got := bs[len(bs)-1]; got.Outcome != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got.Outcome, tc.want)
		}
	}
	// The report rides ahead of the sweep, and is informational.
	bs := orphanedVolumes(ctx, &fakeEC2{}, false, &bytes.Buffer{}, "c", "r")
	if len(bs) != 2 || !bs[0].Informational || bs[0].Name != "unprovable EBS volumes" {
		t.Errorf("want the informational report then the sweep, got %+v", bs)
	}
}

// The unprovable-volume report's own query failing is said, not silent: "none" and "the
// report did not run" read identically as nothing.
func TestReportUnprovableVolumes_ReportsWhatItDid(t *testing.T) {
	ctx := context.Background()
	var out bytes.Buffer
	b := reportUnprovableVolumes(ctx, &fakeEC2{queryErr: errors.New("AccessDenied")}, &out, "c", "r")
	if b.Outcome != NotRun || !b.Informational || !strings.Contains(out.String(), "did NOT run") {
		t.Errorf("a failed query: got %s %v\n%s", b.Outcome, b.Informational, out.String())
	}
	out.Reset()
	if b := reportUnprovableVolumes(ctx, &fakeEC2{unprovable: "not json"}, &out, "c", "r"); b.Outcome != NotRun ||
		!strings.Contains(out.String(), "did NOT run") {
		t.Errorf("unreadable: got %s\n%s", b.Outcome, out.String())
	}
	if b := reportUnprovableVolumes(ctx, &fakeEC2{unprovable: ""}, &bytes.Buffer{}, "c", "r"); b.Outcome != FoundNothing {
		t.Errorf("empty: got %s", b.Outcome)
	}
	if b := reportUnprovableVolumes(ctx, &fakeEC2{unprovable: `[]`}, &bytes.Buffer{}, "c", "r"); b.Outcome != FoundNothing {
		t.Errorf("none: got %s", b.Outcome)
	}
	b = reportUnprovableVolumes(ctx, &fakeEC2{unprovable: `[{"VolumeId":"vol-9","Size":5,"Tags":[{"Key":"CSIVolumeName","Value":"p"}]}]`},
		&bytes.Buffer{}, "c", "r")
	if b.Outcome != LeftBehind || !strings.Contains(b.Detail, "vol-9") {
		t.Errorf("unprovable: got %s %q", b.Outcome, b.Detail)
	}
}
