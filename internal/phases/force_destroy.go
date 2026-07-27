package phases

import (
	"context"
	"fmt"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/engine"
)

// forceDestroyBucketComponents is the set of landing-zone components rackctl may apply
// that declare force_destroy_buckets. Development always allows teardown; elsewhere the
// variable is the only channel (no live leaf sets it), so rackctl must inject it.
//
// Order among these does not matter for the permitting apply — model-import has no
// dependency block, and agent-iam / cluster-addons / druid only need cluster+secrets
// state which is still live when this runs (before any destroy). The reverse-destroy
// loop below still owns the teardown order.
var forceDestroyBucketComponents = map[string]bool{
	"agent-iam":      true, // access-logs, model-artifacts, eval-reports
	"cluster-addons": true, // velero, loki, tempo, argo-workflows
	"model-import":   true, // staging bucket
	"druid":          true, // per-tenant buckets + Aurora skip_final_snapshot (O1)
}

// ForceDestroyBucketComponents returns the CoreComponents of cfg that accept
// force_destroy_buckets, in apply order. Empty when none of them are gated on.
func ForceDestroyBucketComponents(cfg *config.Config) []string {
	var out []string
	for _, c := range CoreComponents(cfg) {
		if forceDestroyBucketComponents[c] {
			out = append(out, c)
		}
	}
	return out
}

// PermitBucketTeardown applies each component that owns non-emptyable buckets with
// TF_VAR_force_destroy_buckets=true, so the subsequent destroy can empty them.
//
// force_destroy has no effect until a successful apply has landed it in state. Injecting
// the variable only on the destroy path appears to work and then fails on BucketNotEmpty
// — the flag must be set on an apply that precedes the teardown. That is why this is a
// separate step, and why a dry-run prints both acts and says why there are two.
//
// velero_backup_policy is the composition upstream documents as the safe order (copy
// recovery points to the central vault first, then empty the local bucket). rackctl does
// not apply the backup component that owns the plan keys, so it is unreachable here; the
// note says so rather than pretending --force-buckets preserves restore points.
func PermitBucketTeardown(ctx context.Context, st *engine.State) error {
	comps := ForceDestroyBucketComponents(st.Config)
	if len(comps) == 0 {
		return nil
	}

	note(st, "force-buckets: two acts, not one. force_destroy has no effect until an apply has "+
		"landed it in state — applying %v with TF_VAR_force_destroy_buckets=true first, then "+
		"destroying. Injecting the flag only on the destroy path fails on BucketNotEmpty", comps)
	note(st, "force-buckets: this empties the local velero/loki/tempo/artifacts buckets. If the "+
		"restore points matter, set velero_backup_policy on the backup component first so the "+
		"central plan copies them to the backup account's DR region — that path is not reachable "+
		"through rackctl (no config field, and rackctl does not apply backup)")
	if st.Config.Environment != config.EnvDev {
		note(st, "force-buckets: environment is %s — development always allows teardown without this "+
			"flag; outside development the permitting apply is what makes destroy reliable",
			st.Config.Environment)
	}

	st.Runner.Dir = st.Repos.LandingZone
	for _, c := range comps {
		note(st, "force-buckets: apply %s with TF_VAR_force_destroy_buckets=true", c)
		if err := applyWith(ctx, st, c, "TF_VAR_force_destroy_buckets=true"); err != nil {
			return fmt.Errorf("force-buckets permitting apply of %s: %w", c, err)
		}
	}
	return nil
}

// applyWith is apply plus extra TF_VARs scoped to this invocation only.
func applyWith(ctx context.Context, st *engine.State, component string, extraEnv ...string) error {
	env, err := componentEnv(ctx, st, component, "apply")
	if err != nil {
		return err
	}
	return tg(ctx, st, "apply", component, append(env, extraEnv...)...)
}
