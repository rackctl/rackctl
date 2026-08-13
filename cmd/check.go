package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/doctor"
	"github.com/rackctl/rackctl/internal/exec"
	"github.com/rackctl/rackctl/internal/preflight"
	"github.com/rackctl/rackctl/internal/ui"
)

var checkConfig string

// check answers "is this platform in a state I can act on" — one command, whichever side of the
// install you are standing on.
//
// It was two: preflight ("can an install succeed?", no cluster needed) and doctor ("is the
// provisioned platform healthy?", needs one). The split is real and the two sets of checks stay
// distinct — but it was the WRONG thing to make the operator choose, because choosing correctly
// requires already knowing the answer to "is there a cluster", which is a thing the tool can
// simply look up.
//
// So it looks it up. No cluster: the pre-spend checks, which is all that can be true yet. A live
// cluster: those PLUS the health assertions, because on a running platform both questions are
// meaningful — preflight's collision checks correctly report a re-apply rather than a conflict,
// and doctor's invariants are the ones that catch a cluster that provisioned green and does not
// work.
//
// Exits non-zero when anything failed, so it gates a deploy.
var checkCmd = &cobra.Command{
	Use:   "check",
	Short: "Check whether an install can succeed, and whether a running platform is healthy",
	Long: `Assert the things that are knowable right now.

With no cluster, that is the pre-spend set: can this install succeed at all? A globally
unique bucket name already taken, Terraform state describing a cluster that was deleted,
a KMS alias orphaned by a scheduled key deletion, a catalog fork behind upstream, a
Kubernetes version EKS will not accept from where this cluster is now. None of those are
cloud failures — they are collisions with the wreckage of a previous attempt, and a
machine enumerates them in seconds.

With a live cluster, it also asserts the invariants of a provisioned platform. Each one
corresponds to a failure that has shipped a broken cluster while every surface reported
success: an app-of-apps syncing from the wrong GitHub org, ApplicationSets erroring so
silently they generated nothing to notice, a metrics collector failing every write,
dashboards that never rendered, a node pool tuned to evict half the fleet at once.

Read-only. It never fixes what it finds — a tool that mutates the account it is auditing
cannot be trusted to audit it. It names the remedy and exits non-zero.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(checkConfig)
		if err != nil {
			return err
		}
		ctx := context.Background()

		if err := exec.RequireTools("tofu", "terragrunt", "kubectl", "helm", "aws", "git", "gh"); err != nil {
			fmt.Println(ui.Fail(err.Error()))
			return err
		}

		fmt.Println(ui.Title(fmt.Sprintf("rackctl check — %s · %s · %s",
			cfg.Org.Name, cfg.Cloud.Region, cfg.Environment)))
		fmt.Println(ui.OK("required tools present"))
		fmt.Println()

		// The pre-spend set. Its runner discards output because the checks are queries and
		// their verdict IS the output.
		q := exec.New(io.Discard)
		q.Env = awsEnv(cfg)
		results := preflight.Run(ctx, &preflight.Env{Cfg: cfg, Run: q})
		printResults(results)
		failed := preflight.Failed(results)

		// The health set, only where it can mean anything. The probe reads the kubeconfig
		// rather than calling `describe-cluster`, because the question is whether THIS shell
		// resolves the cluster — one that exists in EKS but is unreachable from here cannot
		// have its in-cluster invariants asserted, and pretending otherwise produces failures
		// that are about the kubeconfig rather than about the platform.
		run := exec.New(os.Stdout)
		run.Env = awsEnv(cfg) // same identity as the preflight runner above, not the ambient one

		// Reachability is not enough — the cluster has to be the RIGHT one. `kubectl get
		// nodes` answers "can this shell reach a cluster", and doctor then asserted the
		// platform's invariants against whatever that was. Running `rackctl check -c
		// staging.yaml` from a shell pointed at development reported development's health
		// under staging's title, in both directions: a healthy answer about the wrong
		// cluster, or failures blamed on a platform that was never examined.
		//
		// check is read-only, so it refuses rather than repointing. `rackctl destroy`
		// repoints because it is about to act; a diagnostic must not rewrite the
		// operator's kubeconfig as a side effect of being run.
		fmt.Println()
		switch got, err := currentKubeCluster(ctx, run); {
		case err != nil || got == "":
			fmt.Println(ui.Step("no reachable cluster — platform health not asserted (nothing is provisioned yet, " +
				"or this shell is not pointed at it)"))
		case got != cfg.ClusterName():
			fmt.Println(ui.Warn(fmt.Sprintf(
				"kubectl is pointed at %q but this config is for %q — platform health NOT asserted. "+
					"Run `aws eks update-kubeconfig --name %s --region %s --profile %s` to check this one.",
				got, cfg.ClusterName(), cfg.ClusterName(), cfg.Cloud.Region, cfg.Cloud.Profile)))
			failed = true
		default:
			fmt.Println(ui.OK("cluster reachable — asserting platform health"))
			health := doctor.Run(ctx, &doctor.Env{Cfg: cfg, Run: run})
			printResults(health)
			failed = failed || doctor.Failed(health)
		}

		fmt.Println()
		if failed {
			return fmt.Errorf("checks failed — clear the above before provisioning")
		}
		fmt.Println(ui.OK("all checks clear"))
		return nil
	},
}

func init() {
	checkCmd.Flags().StringVarP(&checkConfig, "config", "c", "rackctl.yaml", "path to rackctl.yaml")
}

// currentKubeCluster reports the EKS cluster the ambient kubeconfig resolves to,
// without changing it.
func currentKubeCluster(ctx context.Context, run *exec.Runner) (string, error) {
	out, err := run.Capture(ctx, "kubectl", "config", "view", "--minify",
		"-o", "jsonpath={.clusters[0].name}")
	if err != nil {
		return "", err
	}
	return eksClusterName(strings.TrimSpace(out)), nil
}

// eksClusterName pulls the cluster name out of a kubeconfig cluster entry. EKS
// writes the ARN (arn:aws:eks:<region>:<account>:cluster/<name>); anything else is
// returned unchanged, so a non-EKS context is reported as itself and compared —
// and therefore refused — rather than being parsed into something that happens to
// match the configured name.
func eksClusterName(entry string) string {
	if i := strings.LastIndex(entry, ":cluster/"); i >= 0 {
		return entry[i+len(":cluster/"):]
	}
	return entry
}
