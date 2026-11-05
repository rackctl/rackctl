package cmd

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/exec"
)

// A command's runner re-assumes the role before every mutating call and reuses the cached
// session for reads. A session cached for reads re-assumed only inside its last ten minutes,
// and a mutating call can run for an hour.
func TestBindIdentity_EveryMutationStartsWithAFreshSession(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "sts.log")
	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	script := `#!/bin/sh
if [ "$2" = "assume-role" ]; then
  echo assume >> ` + log + `
  echo '{"AccessKeyId":"ASIAFAKE","SecretAccessKey":"s","SessionToken":"t","Expiration":"` + exp + `"}'
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "aws"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	prev := identity
	identity = nil
	t.Cleanup(func() { identity = prev })

	cfg := config.Default()
	cfg.Cloud.Profile = "test"
	cfg.Cloud.AssumeRole = &config.AssumeRole{RoleARN: "arn:aws:iam::111111111111:role/rackctl"}
	run := exec.New(io.Discard)
	ctx := context.Background()
	if _, err := bindIdentity(ctx, cfg, run); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := run.Capture(ctx, "aws", "sts", "get-caller-identity"); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := run.Run(ctx, "aws", "s3", "ls"); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(log)
	if got := strings.Count(string(b), "assume"); got != 3 {
		t.Errorf("assume-role ran %d times, want 3: once at bind, none for the two reads, "+
			"and once for each of the two mutations", got)
	}
}
