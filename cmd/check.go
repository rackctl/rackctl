package cmd

import (
	"context"
	"encoding/json"
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

var (
	checkConfig string
	checkOutput string
)

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
		if checkOutput != "text" && checkOutput != "json" {
			return withExit(ExitConfig, fmt.Errorf("--output %q is not one of: text, json", checkOutput))
		}
		cfg, err := config.Load(checkConfig)
		if err != nil {
			return withExit(ExitConfig, err)
		}
		ctx := cmd.Context()
		// Under --output json the prose goes to STDERR and stdout carries the document
		// alone, so `rackctl check --output json | jq` works without the caller having to
		// filter a banner out of its own input.
		//
		// Redirected, not suppressed. An operator who asks for JSON is still watching a
		// terminal, and a command that prints nothing at all while it works looks hung —
		// the machine-readable half is a second consumer, not a reason to stop talking to
		// the first. The per-check results are the exception: they are IN the document, so
		// echoing them to stderr would be the same content twice.
		say := func(a ...any) {
			if checkOutput == "text" {
				fmt.Println(a...)
				return
			}
			fmt.Fprintln(os.Stderr, a...)
		}

		if err := exec.RequireTools("tofu", "terragrunt", "kubectl", "helm", "aws", "git", "gh"); err != nil {
			say(ui.Fail(err.Error()))
			return err
		}

		say(ui.Title(commandTitle("check", cfg)))
		say(ui.OK("required tools present"))
		say()

		// The pre-spend set. Its runner discards output because the checks are queries and
		// their verdict IS the output.
		q := exec.New(io.Discard)
		if _, err := bindIdentity(ctx, cfg, q); err != nil {
			return err
		}
		results := preflight.Run(ctx, &preflight.Env{Cfg: cfg, Run: q})
		if checkOutput == "text" {
			printResults(results)
		}
		failed := preflight.Failed(results)
		var health []doctor.Result
		// Assigned by every arm of the switch below, which is exhaustive. Left undeclared
		// rather than seeded with a default, so a future arm that forgets to set it shows
		// up as an empty string in the report rather than silently claiming the state the
		// seed happened to name.
		var clusterState string

		// The health set, only where it can mean anything. The probe reads the kubeconfig
		// rather than calling `describe-cluster`, because the question is whether THIS shell
		// resolves the cluster — one that exists in EKS but is unreachable from here cannot
		// have its in-cluster invariants asserted, and pretending otherwise produces failures
		// that are about the kubeconfig rather than about the platform.
		run := exec.New(os.Stdout)
		// The SAME identity as the preflight runner above, not the ambient one — the two
		// halves of one command must not ask AWS as different principals. Bound rather than
		// copied so both re-resolve from the one session cache.
		if _, err := bindIdentity(ctx, cfg, run); err != nil {
			return err
		}

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
		say()
		switch got, err := currentKubeCluster(ctx, run); {
		case err != nil || got == "":
			clusterState = "unreachable"
			say(ui.Step("no reachable cluster — platform health not asserted (nothing is provisioned yet, " +
				"or this shell is not pointed at it)"))
		case got != cfg.ClusterName():
			clusterState = "wrong-cluster"
			say(ui.Warn(fmt.Sprintf(
				"kubectl is pointed at %q but this config is for %q — platform health NOT asserted. "+
					"Run `aws eks update-kubeconfig --name %s --region %s --profile %s` to check this one.",
				got, cfg.ClusterName(), cfg.ClusterName(), cfg.Cloud.Region, cfg.Cloud.Profile)))
			failed = true
		default:
			clusterState = "asserted"
			say(ui.OK("cluster reachable — asserting platform health"))
			health = doctor.Run(ctx, &doctor.Env{Cfg: cfg, Run: run})
			if checkOutput == "text" {
				printResults(health)
			}
			failed = failed || doctor.Failed(health)
		}

		if checkOutput == "json" {
			if err := emitCheckJSON(cfg, clusterState, results, health, failed); err != nil {
				return err
			}
		}

		say()
		if failed {
			return fmt.Errorf("checks failed — clear the above before provisioning")
		}
		say(ui.OK("all checks clear"))
		return nil
	},
}

// checkReport is the shape `rackctl check --output json` emits. It is a contract: fields
// are added, never renamed or removed, and TestCheckJSON_ShapeIsStable pins it.
type checkReport struct {
	Cluster     string `json:"cluster"`
	Environment string `json:"environment"`
	Account     string `json:"account"`
	Region      string `json:"region"`
	// ClusterHealth is asserted, unreachable, wrong-cluster or not-asserted. It says
	// whether the platform half of this report means anything, which a caller cannot infer
	// from an empty results list — no findings and no look are the same document otherwise.
	ClusterHealth string        `json:"clusterHealth"`
	Passed        bool          `json:"passed"`
	Preflight     []checkResult `json:"preflight"`
	Platform      []checkResult `json:"platform"`
}

type checkResult struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

func toCheckResults(in []doctor.Result) []checkResult {
	out := make([]checkResult, 0, len(in))
	for _, r := range in {
		out = append(out, checkResult{Name: r.Name, Status: strings.ToLower(r.Status.String()), Detail: r.Detail})
	}
	return out
}

func emitCheckJSON(cfg *config.Config, clusterHealth string, pre, platform []doctor.Result, failed bool) error {
	doc := checkReport{
		Cluster:       cfg.ClusterName(),
		Environment:   string(cfg.Environment),
		Account:       cfg.Cloud.AccountID,
		Region:        cfg.Cloud.Region,
		ClusterHealth: clusterHealth,
		Passed:        !failed,
		Preflight:     toCheckResults(pre),
		Platform:      toCheckResults(platform),
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

func init() {
	checkCmd.Flags().StringVarP(&checkConfig, "config", "c", "rackctl.yaml", "path to rackctl.yaml")
	checkCmd.Flags().StringVar(&checkOutput, "output", "text",
		"output format: text, or json for a machine-readable report on stdout")
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
