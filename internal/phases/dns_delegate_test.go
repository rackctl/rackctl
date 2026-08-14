package phases

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/engine"
	"github.com/rackctl/rackctl/internal/exec"
)

// fakeRoute53 puts an `aws` on PATH that answers the four route53 calls
// delegateSubdomain makes, and appends every change-resource-record-sets
// invocation to a log file so the test can assert on what was WRITTEN.
//
// A real fake binary rather than a note-asserting stub: the thing under test is
// whether a write happens at all, and a message describing a write cannot witness
// one. Returns the state and the path of the invocation log.
func fakeRoute53(t *testing.T, existingNS []string) (*engine.State, string, *strings.Builder) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "changes.log")

	// The child zone's own name servers — what the delegation should end up naming.
	const childNS = `["ns-1.awsdns.com","ns-2.awsdns.org"]`
	rrset := "[]"
	if len(existingNS) > 0 {
		rrset = `["` + strings.Join(existingNS, `","`) + `"]`
	}

	aws := fmt.Sprintf(`#!/bin/sh
if [ "$2" = "list-hosted-zones-by-name" ]; then echo "/hostedzone/Z123"; exit 0; fi
if [ "$2" = "get-hosted-zone" ]; then echo '%s'; exit 0; fi
if [ "$2" = "list-resource-record-sets" ]; then echo '%s'; exit 0; fi
if [ "$2" = "change-resource-record-sets" ]; then echo "$@" >> %s; exit 0; fi
exit 0
`, childNS, rrset, logPath)
	if err := os.WriteFile(filepath.Join(dir, "aws"), []byte(aws), 0o755); err != nil {
		t.Fatalf("write fake aws: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := config.Default()
	cfg.DNS = &config.DNS{HostedZone: "platform.acme.example.com"}
	var notes strings.Builder
	return &engine.State{Config: cfg, Runner: exec.New(&notes)}, logPath, &notes
}

func changeWrites(t *testing.T, logPath string) string {
	t.Helper()
	b, err := os.ReadFile(logPath)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A re-apply must not rewrite a delegation that already names the right servers.
// The write was unconditional, so every run reissued the same UPSERT.
func TestDelegateSubdomainIsANoOpWhenAlreadyCorrect(t *testing.T) {
	st, logPath, _ := fakeRoute53(t, []string{"ns-1.awsdns.com", "ns-2.awsdns.org"})

	delegateSubdomain(context.Background(), st)

	if got := changeWrites(t, logPath); got != "" {
		t.Errorf("the delegation already named these servers; nothing should have been written.\ngot: %s", got)
	}
}

// Order and trailing dots are not differences — Route53 promises no ordering, and
// a delegation that came back as "NS-1.AWSDNS.COM." is the same delegation.
func TestDelegateSubdomainIgnoresOrderAndTrailingDots(t *testing.T) {
	st, logPath, _ := fakeRoute53(t, []string{"NS-2.AWSDNS.ORG.", "ns-1.awsdns.com."})

	delegateSubdomain(context.Background(), st)

	if got := changeWrites(t, logPath); got != "" {
		t.Errorf("same servers in a different order/case must be a no-op.\ngot: %s", got)
	}
}

// When the parent already delegates this name somewhere ELSE, the replacement still
// happens — a child zone recreated with new name servers is the case UPSERT exists
// for — but the values being replaced must be named, or an overwrite of a
// delegation rackctl does not own is silent and unrecoverable.
func TestDelegateSubdomainNamesWhatItReplaces(t *testing.T) {
	st, logPath, notes := fakeRoute53(t, []string{"ns-999.someone-else.net"})

	delegateSubdomain(context.Background(), st)

	if got := changeWrites(t, logPath); !strings.Contains(got, "change-resource-record-sets") {
		t.Fatalf("a differing delegation must still be corrected.\ngot: %q", got)
	}
	if !strings.Contains(notes.String(), "ns-999.someone-else.net") {
		t.Errorf("the replaced name servers must be recorded before they are overwritten — "+
			"otherwise the previous delegation cannot be restored.\nnotes: %s", notes.String())
	}
}

// A first delegation — nothing in the parent — writes without any warning noise.
func TestDelegateSubdomainWritesWhenNothingIsThere(t *testing.T) {
	st, logPath, notes := fakeRoute53(t, nil)

	delegateSubdomain(context.Background(), st)

	if got := changeWrites(t, logPath); !strings.Contains(got, "change-resource-record-sets") {
		t.Fatalf("a first delegation must be written.\ngot: %q", got)
	}
	if strings.Contains(notes.String(), "already has an NS record") {
		t.Errorf("nothing was there to replace; the replacement warning must not fire.\nnotes: %s", notes.String())
	}
}

func TestSameServers(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b []string
		want bool
	}{
		{"identical", []string{"a", "b"}, []string{"a", "b"}, true},
		{"reordered", []string{"b", "a"}, []string{"a", "b"}, true},
		{"trailing dots", []string{"a.", "b."}, []string{"a", "b"}, true},
		{"case", []string{"A", "B"}, []string{"a", "b"}, true},
		{"different member", []string{"a", "c"}, []string{"a", "b"}, false},
		{"different length", []string{"a"}, []string{"a", "b"}, false},
		{"both empty is not a match", nil, nil, false},
		{"empty existing", nil, []string{"a"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameServers(tc.a, tc.b); got != tc.want {
				t.Errorf("sameServers(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}
