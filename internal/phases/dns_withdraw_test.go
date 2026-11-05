package phases

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/engine"
	"github.com/rackctl/rackctl/internal/exec"
)

const (
	childNS   = `["ns-1.awsdns-01.com","ns-2.awsdns-02.net"]`
	ownRRSet  = `{"Name":"acme.example.com.","Type":"NS","TTL":300,"ResourceRecords":[{"Value":"ns-2.awsdns-02.net."},{"Value":"ns-1.awsdns-01.com"}]}`
	elsewhere = `{"Name":"acme.example.com.","Type":"NS","TTL":300,"ResourceRecords":[{"Value":"ns-9.other.example"}]}`
)

// withdraw runs WithdrawSubdomainDelegation against a fake Route53 described by env, and
// returns what it printed and every change-resource-record-sets it sent.
func withdraw(t *testing.T, dryRun bool, zone string, env map[string]string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	logf := filepath.Join(dir, "changes")
	script := `#!/bin/sh
case "$*" in
  *"list-hosted-zones-by-name --dns-name acme.example.com."*) echo "${CHILD_ID:-None}" ;;
  *list-hosted-zones-by-name*) echo "${PARENT_ID:-None}" ;;
  *list-resource-record-sets*) [ -n "$FAIL_RRSET" ] && { echo AccessDenied >&2; exit 254; }; printf '%s\n' "$RRSET" ;;
  *get-hosted-zone*) [ -n "$FAIL_NS" ] && { echo AccessDenied >&2; exit 254; }; printf '%s\n' "$CHILD_NS" ;;
  *change-resource-record-sets*) echo "$*" >> ` + logf + `; [ -n "$FAIL_CHANGE" ] && exit 254 ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "aws"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, k := range []string{"CHILD_ID", "PARENT_ID", "RRSET", "CHILD_NS", "FAIL_RRSET", "FAIL_NS", "FAIL_CHANGE"} {
		t.Setenv(k, env[k])
	}
	var out strings.Builder
	run := exec.New(&out)
	run.DryRun, run.ReadAttempts = dryRun, 1
	cfg := config.Default()
	if zone != "" {
		cfg.DNS = &config.DNS{HostedZone: zone}
	}
	WithdrawSubdomainDelegation(context.Background(), &engine.State{Config: cfg, Runner: run})
	b, _ := os.ReadFile(logf)
	return out.String(), string(b)
}

// The record is deleted only when it points at the name servers of this account's zone by
// the same name, which is what delegateSubdomain wrote.
func TestWithdraw_DeletesOnlyTheDelegationItCanProve(t *testing.T) {
	owned := map[string]string{"PARENT_ID": "/hostedzone/P", "CHILD_ID": "/hostedzone/C", "RRSET": ownRRSet, "CHILD_NS": childNS}
	out, changes := withdraw(t, false, "acme.example.com", owned)
	if !strings.Contains(changes, `"Action":"DELETE"`) || !strings.Contains(changes, `"TTL":300`) {
		t.Fatalf("an owned delegation must be deleted, matching the record exactly: %q\n%s", changes, out)
	}
	if !strings.Contains(out, "withdrew acme.example.com's delegation") {
		t.Errorf("the withdrawal must be said:\n%s", out)
	}

	for _, c := range []struct {
		name string
		env  map[string]string
		why  string
	}{
		{"the child zone is gone", map[string]string{"PARENT_ID": "/hostedzone/P", "RRSET": ownRRSet},
			"holds no public zone acme.example.com any more"},
		{"it points somewhere else", map[string]string{"PARENT_ID": "/hostedzone/P", "CHILD_ID": "/hostedzone/C", "RRSET": elsewhere, "CHILD_NS": childNS},
			"they are not the name servers"},
		{"the child's servers cannot be read", map[string]string{"PARENT_ID": "/hostedzone/P", "CHILD_ID": "/hostedzone/C", "RRSET": ownRRSet, "FAIL_NS": "1"},
			"could not be read"},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, changes := withdraw(t, false, "acme.example.com", c.env)
			if changes != "" {
				t.Fatalf("a delegation that cannot be proved ours was deleted: %q", changes)
			}
			for _, want := range []string{"leaving acme.example.com's NS record", c.why,
				"aws route53 change-resource-record-sets --hosted-zone-id /hostedzone/P --change-batch"} {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q:\n%s", want, out)
				}
			}
		})
	}
}

// A dry-run reads the record and the zone for real, and stops short of the delete.
func TestWithdraw_ADryRunShowsWhatItWouldWithdraw(t *testing.T) {
	out, changes := withdraw(t, true, "acme.example.com",
		map[string]string{"PARENT_ID": "/hostedzone/P", "CHILD_ID": "/hostedzone/C", "RRSET": ownRRSet, "CHILD_NS": childNS})
	if changes != "" {
		t.Fatalf("a dry-run changed a record: %q", changes)
	}
	if !strings.Contains(out, "(destroy) would withdraw acme.example.com's delegation from example.com") {
		t.Errorf("a dry-run must say what it found:\n%s", out)
	}
}

// Every path that withdraws nothing says why, rather than returning silently.
func TestWithdraw_EveryPathThatDoesNothingSaysSo(t *testing.T) {
	for _, c := range []struct {
		name, zone string
		env        map[string]string
		want       string
	}{
		{"an apex domain", "example.com", nil, "is an apex domain"},
		{"a parent this account does not host", "acme.example.com", nil, "is not a public hosted zone in this account"},
		{"no record", "acme.example.com", map[string]string{"PARENT_ID": "/hostedzone/P", "RRSET": "null"}, "holds no NS record"},
		{"an unreadable parent", "acme.example.com", map[string]string{"PARENT_ID": "/hostedzone/P", "FAIL_RRSET": "1"}, "leaving whatever is there"},
		{"an unreadable record", "acme.example.com", map[string]string{"PARENT_ID": "/hostedzone/P", "RRSET": "{"}, "could not be read"},
		{"a delete that fails", "acme.example.com", map[string]string{"PARENT_ID": "/hostedzone/P", "CHILD_ID": "/hostedzone/C",
			"RRSET": ownRRSet, "CHILD_NS": childNS, "FAIL_CHANGE": "1"}, "could not withdraw"},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, _ := withdraw(t, false, c.zone, c.env)
			if !strings.Contains(out, c.want) {
				t.Errorf("missing %q:\n%s", c.want, out)
			}
		})
	}
	if out, _ := withdraw(t, false, "", nil); out != "" {
		t.Errorf("no dns block, nothing to say: %q", out)
	}
}
