package reap

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rackctl/rackctl/internal/exec"
)

// workloads is a list of StatefulSets and Deployments with the given replica count: one that
// mounts a claim template, one that mounts a PVC volume, one that mounts none, and the EBS
// CSI controller in kube-system, which has to stay up to release the volumes.
func workloads(replicas int) string {
	r := func(s string) string { return strings.ReplaceAll(s, "R", string(rune('0'+replicas))) }
	return r(`{"items":[` +
		`{"kind":"StatefulSet","metadata":{"name":"loki","namespace":"loki"},"spec":{"volumeClaimTemplates":[{}]},"status":{"replicas":R}},` +
		`{"kind":"Deployment","metadata":{"name":"kagent-db","namespace":"kagent"},"spec":{"template":{"spec":{"volumes":[{"persistentVolumeClaim":{"claimName":"data"}}]}}},"status":{"replicas":R}},` +
		`{"kind":"Deployment","metadata":{"name":"web","namespace":"apps"},"spec":{"template":{"spec":{"volumes":[{"emptyDir":{}}]}}},"status":{"replicas":R}},` +
		`{"kind":"Deployment","metadata":{"name":"ebs-csi-controller","namespace":"kube-system"},"spec":{"template":{"spec":{"volumes":[{"persistentVolumeClaim":{"claimName":"x"}}]}}},"status":{"replicas":R}}` +
		`]}`)
}

// scaleKubectl puts a kubectl on PATH whose first workload list answers first, every later
// one answers later, and whose scale runs scale. It returns the log of scale calls.
func scaleKubectl(t *testing.T, first, later, scale string) func() string {
	t.Helper()
	dir := t.TempDir()
	logf, seen := filepath.Join(dir, "scales"), filepath.Join(dir, "seen")
	script := `#!/bin/sh
case "$*" in
  *"get statefulsets,deployments"*)
    if [ -f ` + seen + ` ]; then ` + later + `; else : > ` + seen + `; ` + first + `; fi ;;
  *" scale "*) echo "$*" >> ` + logf + `; ` + scale + ` ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	prev := settle
	settle.tries, settle.every = 3, time.Millisecond
	t.Cleanup(func() { settle = prev })
	return func() string { b, _ := os.ReadFile(logf); return string(b) }
}

func echo(s string) string { return "echo '" + s + "'" }

// Every workload that mounts a claim is scaled to zero, and the scale-down waits until the
// re-read says each is at zero, so pvc-protection lets the delete finish. kube-system and
// workloads that mount nothing are left running.
func TestScaleVolumeHolders_StopsWhatMountsAClaimAndWaitsForZero(t *testing.T) {
	scales := scaleKubectl(t, echo(workloads(1)), echo(workloads(0)), "exit 0")
	var out bytes.Buffer
	if p := scaleVolumeHolders(context.Background(), exec.New(&out), &out); p != "" {
		t.Fatalf("got %q\n%s", p, out.String())
	}
	got := scales()
	for _, want := range []string{"-n loki scale statefulset/loki --replicas=0", "-n kagent scale deployment/kagent-db --replicas=0"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	for _, never := range []string{"web", "ebs-csi-controller"} {
		if strings.Contains(got, never) {
			t.Errorf("%s mounts no claim or runs the CSI driver, and was scaled:\n%s", never, got)
		}
	}
}

// Each way the scale-down can fall short is returned, so the reap reports it.
func TestScaleVolumeHolders_SaysWhatItCouldNotEstablish(t *testing.T) {
	for _, c := range []struct{ name, first, later, scale, want string }{
		{"the list cannot be read", "echo Forbidden >&2; exit 1", "exit 1", "exit 0", "could not be listed"},
		{"the list is not a list", echo(`{}`), "exit 1", "exit 0", "could not be listed"},
		{"the list is not JSON", "echo garbage", "exit 1", "exit 0", "could not be listed"},
		{"a scale fails", echo(workloads(1)), echo(workloads(0)), "exit 1", "could not scale"},
		{"a workload never reaches zero", echo(workloads(1)), echo(workloads(1)), "exit 0", "still running after the scale-down: loki/statefulset/loki"},
		{"the re-read fails", echo(workloads(1)), "exit 1", "exit 0", "still running after the scale-down: unknown"},
	} {
		t.Run(c.name, func(t *testing.T) {
			scaleKubectl(t, c.first, c.later, c.scale)
			if p := scaleVolumeHolders(context.Background(), exec.New(&bytes.Buffer{}), &bytes.Buffer{}); !strings.Contains(p, c.want) {
				t.Errorf("got %q, want it to contain %q", p, c.want)
			}
		})
	}
	t.Run("nothing mounts a claim", func(t *testing.T) {
		scales := scaleKubectl(t, echo(`{"items":[]}`), "exit 1", "exit 0")
		if p := scaleVolumeHolders(context.Background(), exec.New(&bytes.Buffer{}), &bytes.Buffer{}); p != "" || scales() != "" {
			t.Errorf("got %q, scaled %q", p, scales())
		}
	})
	t.Run("interrupted", func(t *testing.T) {
		scaleKubectl(t, echo(workloads(1)), echo(workloads(1)), "exit 0")
		settle.every = time.Hour
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(200 * time.Millisecond); cancel() }()
		if p := scaleVolumeHolders(ctx, exec.New(&bytes.Buffer{}), &bytes.Buffer{}); p != "the scale-down was interrupted" {
			t.Errorf("got %q", p)
		}
	})
}

// The scale-down runs after a verified disarm and before the PVC delete; without a verified
// disarm it does not run, since an armed ArgoCD would scale the workloads straight back up.
func TestAll_ScalesDownBeforeThePVCDeleteOnlyAfterAVerifiedDisarm(t *testing.T) {
	for _, c := range []struct {
		name, apps string
		scaled     bool
	}{
		{"verified", `{"items":[]}`, true},
		{"still armed", `{"items":[{"metadata":{"name":"loki"},"spec":{"syncPolicy":{"automated":{}}}}]}`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			calls := filepath.Join(dir, "calls")
			script := `#!/bin/sh
printf '%s\n' "$*" >> ` + calls + `
case "$*" in
  *"get applications -o jsonpath"*) echo loki ;;
  *"get applications -o json"*) echo '` + c.apps + `' ;;
  *"get statefulsets,deployments"*) echo '` + workloads(0) + `' ;;
esac
exit 0
`
			if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			var out bytes.Buffer
			_ = All(context.Background(), exec.New(&out), &out, "c", "r")
			b, _ := os.ReadFile(calls)
			lines := strings.Split(string(b), "\n")
			scaleAt, pvcAt := -1, -1
			for i, l := range lines {
				if scaleAt < 0 && strings.Contains(l, "scale statefulset/loki") {
					scaleAt = i
				}
				if strings.HasPrefix(l, "delete pvc") {
					pvcAt = i
				}
			}
			if c.scaled && (scaleAt < 0 || scaleAt > pvcAt) {
				t.Fatalf("the workloads must be scaled to zero before the PVC delete (scale=%d pvc=%d):\n%s", scaleAt, pvcAt, b)
			}
			if !c.scaled && scaleAt >= 0 {
				t.Fatalf("scaled down behind an ArgoCD that is still armed:\n%s", b)
			}
		})
	}
}
