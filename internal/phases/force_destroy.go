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
	"druid":          true, // per-tenant buckets + Aurora skip_final_snapshot (O1, PARTIAL — see druidTeardownWedged)
}

// druidTeardownWedged reports whether druid's teardown still wedges after the permitting
// apply, which it does everywhere except development.
//
// landing-zone's d7c7ec4 closed two thirds of the druid wedge — force_destroy on the
// per-tenant buckets, and skip_final_snapshot on the Aurora cluster, both gated on
// `local.allow_teardown = var.environment == "development" || var.force_destroy_buckets`.
// It did not touch the third: `deletion_protection = var.tenant_config.deletion_protection`
// (modules/tenant/aurora.tf:94) is wired straight from the tenant map, the component
// default is true (druid/variables.tf:67), and BOTH non-development leaves pin it true.
// The file's own comment concedes the gap — destroy is reachable "once deletion_protection
// is [clear]", and nothing in the permitting apply clears it.
//
// rackctl cannot reach it either. deletion_protection lives inside a map(object(...)), so
// the only ambient channel is a wholesale TF_VAR_tenants that REPLACES the leaf's map —
// taking its rds_min_acu/rds_max_acu/rds_backup_days sizing pins with it. Substituting a
// guessed map for the operator's real one to clear one boolean is precisely the silent
// override this campaign exists to kill.
//
// The order makes it worse than a plain wedge. Act 2 destroys druid's buckets and its
// Aurora in one terragrunt run; the bucket deletes have no dependency on the cluster, so
// they succeed while DeleteDBCluster fails on "Cannot delete protected DB Cluster". The
// deepstorage bucket is the sole durable copy of a tenant's Druid segments and has neither
// versioning nor expiry — so the operator ends up with the segments irreversibly gone, the
// Aurora still standing, and the reverse sweep halted before cluster and network, leaving
// the EKS cluster, the VPC and the NAT gateways billing.
//
// So this is disclosed and refused rather than attempted. Ledger O8 carries the upstream
// ask: gate it on the same lever the other two already use.
func druidTeardownWedged(cfg *config.Config) bool {
	if cfg.Environment == config.EnvDev {
		return false
	}
	for _, c := range ForceDestroyBucketComponents(cfg) {
		if c == "druid" {
			return true
		}
	}
	return false
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

	// Refuse BEFORE anything else, including the notes. Once the permitting apply has run
	// there is nothing to undo, and announcing a two-act plan that is about to be refused
	// reads as though act 1 already happened.
	if druidTeardownWedged(st.Config) {
		return &engine.NoRollbackError{Err: fmt.Errorf(
			"--force-buckets cannot tear druid down in %s, and going ahead would destroy data "+
				"before it failed.\n\n"+
				"landing-zone pins Aurora deletion_protection = true in the %s druid leaf, and the "+
				"permitting apply cannot clear it: it lives inside the tenants map(object), so the "+
				"only override is a wholesale TF_VAR_tenants that would also replace the leaf's "+
				"rds_min_acu/rds_max_acu/rds_backup_days sizing.\n\n"+
				"What would happen if this continued: the druid destroy deletes the per-tenant "+
				"deepstorage, indexlogs and msq buckets (deepstorage has no versioning and no "+
				"expiry — those segments are the only copy), then fails on DeleteDBCluster and "+
				"halts the sweep before cluster and network, leaving the EKS cluster, the VPC and "+
				"the NAT gateways billing.\n\n"+
				"To tear this platform down: clear deletion_protection on the druid tenants out of "+
				"band (modify the Aurora cluster, or set it false in the leaf and apply), then "+
				"re-run. Or set addons.druid: false only AFTER druid is gone — turning it off here "+
				"just removes it from the sweep and strands it.",
			st.Config.Environment, st.Config.Environment)}
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
