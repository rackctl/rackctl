package phases

import (
	"context"
	"errors"
	"testing"

	"github.com/rackctl/rackctl/internal/engine"
)

// assertRefusal fails unless err is a refusal made before anything was spent. Without the
// mark, a first install that stops here exits platform-left-standing — or, with no
// NoRollbackError at all, rolled-back — over a platform that does not exist.
func assertRefusal(t *testing.T, err error) {
	t.Helper()
	var nr *engine.NoRollbackError
	if !errors.As(err, &nr) || !nr.BeforeSpend {
		t.Fatalf("want a refusal before spend (engine.Refuse), got %T: %v", err, err)
	}
}

// A pin that does not resolve stops acquire before anything is provisioned.
func TestPinnedCheckout_AnUnknownRefIsARefusal(t *testing.T) {
	clone := originAndClone(t)
	err := cloneOrUpdate(context.Background(), state(clone), "", clone, "no-such-tag")
	if err == nil {
		t.Fatal("an unknown ref must stop the run")
	}
	assertRefusal(t, err)
}

// The preflight phase checks tools and identity before anything is spent, so its failure is
// a refusal too.
func TestPreflightPhase_AFailureIsARefusal(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	err := preflight{}.Run(context.Background(), &engine.State{Config: baseCfg()})
	if err == nil {
		t.Fatal("a missing tool must stop the preflight phase")
	}
	assertRefusal(t, err)
}
