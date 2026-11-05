package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/doctor"
	"github.com/rackctl/rackctl/internal/engine"
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
			return earlyCheckExit(nil, nil, withExit(ExitConfig, err))
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
			return earlyCheckExit(cfg, nil, err)
		}

		say(ui.Title(commandTitle("check", cfg)))
		say(ui.OK("required tools present"))
		say()

		// The pre-spend set. Its runner discards output because the checks are queries and
		// their verdict IS the output.
		q := exec.New(io.Discard)
		if _, err := bindIdentity(ctx, cfg, q); err != nil {
			say(ui.Fail("could not resolve the identity to check as: " + err.Error()))
			return earlyCheckExit(cfg, nil, err)
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
			say(ui.Fail("could not resolve the identity to check the platform as: " + err.Error()))
			return earlyCheckExit(cfg, results, err)
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
		// Three verdicts, kept apart because they call for different next moves: the
		// platform is unhealthy or its health could not be asserted against a cluster that
		// exists, the install would not succeed, or check could not find out.
		preflightFailed := failed
		var unhealthy, undetermined bool
		// Whether EKS has this config's cluster, when check had to ask: "absent", "present"
		// or "unknown". A reachable cluster is "present" without asking.
		var clusterExists string
		exists := func() engine.PlatformState {
			st := engine.PlatformExists(ctx, &engine.State{Config: cfg, Runner: run})
			clusterExists = existsToken(st)
			return st
		}
		switch entry, err := currentKubeCluster(ctx, run); {
		case err != nil || entry == "":
			// A shell with no kubeconfig entry for the cluster cannot tell "nothing is
			// provisioned yet" from "this shell is not pointed at it". EKS can, so it is
			// asked, and only the first is a pass.
			switch exists() {
			case engine.PlatformAbsent:
				clusterState = "unreachable"
				say(ui.Step("no cluster named " + cfg.ClusterName() + " in EKS yet — platform health not asserted"))
			case engine.PlatformPresent:
				clusterState = "unreachable"
				unhealthy = true
				say(ui.Fail(fmt.Sprintf(
					"%s exists in EKS but this shell cannot reach it — platform health NOT asserted. "+
						"Run `aws eks update-kubeconfig --name %s --region %s --profile %s` to check it.",
					cfg.ClusterName(), cfg.ClusterName(), cfg.Cloud.Region, cfg.Cloud.Profile)))
			default:
				clusterState = "not-asserted"
				undetermined = true
				say(ui.Warn("no reachable cluster, and EKS could not say whether " + cfg.ClusterName() +
					" exists — platform health NOT asserted. Fix the credentials or connectivity and re-run."))
			}
		case kubeClusterMismatch(entry, cfg) != "":
			clusterState = "wrong-cluster"
			unhealthy = true
			exists()
			say(ui.Warn(fmt.Sprintf(
				"%s — platform health NOT asserted. "+
					"Run `aws eks update-kubeconfig --name %s --region %s --profile %s` to check this one.",
				kubeClusterMismatch(entry, cfg), cfg.ClusterName(), cfg.Cloud.Region, cfg.Cloud.Profile)))
		default:
			clusterState = "asserted"
			clusterExists = existsToken(engine.PlatformPresent)
			say(ui.OK("cluster reachable — asserting platform health"))
			health = doctor.Run(ctx, &doctor.Env{Cfg: cfg, Run: run})
			if checkOutput == "text" {
				printResults(health)
			}
			unhealthy = doctor.Failed(health)
		}
		failed = preflightFailed || unhealthy || undetermined

		if checkOutput == "json" {
			doc := newCheckReport(cfg, clusterState, results, health, failed)
			doc.ClusterExists = clusterExists
			if err := writeCheckReport(doc); err != nil {
				return err
			}
		}

		say()
		switch {
		case unhealthy:
			return withExit(ExitUnhealthy, fmt.Errorf(
				"checks failed — the platform is unhealthy, or its health could not be asserted from this shell; clear the above"))
		case preflightFailed:
			return withExit(ExitPreflight, fmt.Errorf("checks failed — this install would not succeed; clear the above before provisioning"))
		case undetermined:
			return fmt.Errorf("check could not establish whether %s exists, so platform health was not asserted", cfg.ClusterName())
		}
		if clusterState == "unreachable" {
			say(ui.OK("pre-spend checks clear — platform health NOT asserted (no cluster yet)"))
			return nil
		}
		say(ui.OK("all checks clear"))
		return nil
	},
}

// existsToken is the clusterExists value for a platform state.
func existsToken(s engine.PlatformState) string {
	switch s {
	case engine.PlatformAbsent:
		return "absent"
	case engine.PlatformPresent:
		return "present"
	default:
		return "unknown"
	}
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
	// Error is why the check could not run to the end, on a run that stopped early: a
	// missing tool, an identity that did not resolve, a config that did not load. Absent on
	// a run that finished. Such a report carries clusterHealth "not-asserted" and passed
	// false, so a caller never mistakes a check that did not run for one that found nothing.
	Error string `json:"error,omitempty"`
	// ClusterExists is whether EKS has this config's cluster: absent, present or unknown.
	// It separates the two meanings of an unreachable cluster — nothing provisioned yet,
	// which passes, and a cluster this shell cannot reach, which does not. Absent on a run
	// that stopped before it could ask.
	ClusterExists string `json:"clusterExists,omitempty"`
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

// newCheckReport builds the document. A nil cfg — a config that did not load — leaves the
// identifying fields empty rather than inventing them.
func newCheckReport(cfg *config.Config, clusterHealth string, pre, platform []doctor.Result, failed bool) checkReport {
	doc := checkReport{
		ClusterHealth: clusterHealth,
		Passed:        !failed,
		Preflight:     toCheckResults(pre),
		Platform:      toCheckResults(platform),
	}
	if cfg != nil {
		doc.Cluster = cfg.ClusterName()
		doc.Environment = string(cfg.Environment)
		doc.Account = cfg.Cloud.AccountID
		doc.Region = cfg.Cloud.Region
	}
	return doc
}

func writeCheckReport(doc checkReport) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

// earlyCheckExit ends a check that stopped before it could finish. Under --output json it
// still writes the document — clusterHealth not-asserted, passed false, whatever preflight
// results were gathered and the reason — because a caller piping stdout into a parser
// otherwise gets an empty stream and has to tell "nothing ran" from a crash by itself. The
// error is returned unchanged, so the exit status is the one it already carried.
func earlyCheckExit(cfg *config.Config, pre []doctor.Result, err error) error {
	if checkOutput != "json" {
		return err
	}
	doc := newCheckReport(cfg, "not-asserted", pre, nil, true)
	doc.Error = err.Error()
	if werr := writeCheckReport(doc); werr != nil {
		return errors.Join(err, werr)
	}
	return err
}

func init() {
	checkCmd.Flags().StringVarP(&checkConfig, "config", "c", "rackctl.yaml", "path to rackctl.yaml")
	checkCmd.Flags().StringVar(&checkOutput, "output", "text",
		"output format: text, or json for a machine-readable report on stdout")
}

// currentKubeCluster reports the cluster entry the ambient kubeconfig resolves to,
// without changing it. For an EKS cluster that is its full ARN, which is what lets the
// caller compare the account and the region as well as the name.
func currentKubeCluster(ctx context.Context, run *exec.Runner) (string, error) {
	out, err := run.Capture(ctx, "kubectl", "config", "view", "--minify",
		"-o", "jsonpath={.clusters[0].name}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// kubeClusterMismatch says why a kubeconfig cluster entry is not this config's cluster, or
// returns "" when it is.
//
// A name is not an identity. Cluster names are unique per account and region, so the same
// name in a sibling account or another region is a different cluster, and asserting its
// health under this config's title reports on a platform nobody asked about. An EKS ARN
// carries the region and the account, so both are compared. Anything that is not an EKS
// ARN is compared as itself, so a kind or minikube context is refused rather than parsed
// into something that happens to match.
func kubeClusterMismatch(entry string, cfg *config.Config) string {
	parts := strings.SplitN(entry, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[2] != "eks" || !strings.HasPrefix(parts[5], "cluster/") {
		if entry == cfg.ClusterName() {
			return ""
		}
		return fmt.Sprintf("kubectl is pointed at %q but this config is for %q", entry, cfg.ClusterName())
	}
	region, account, name := parts[3], parts[4], eksClusterName(entry)
	switch {
	case name != cfg.ClusterName():
		return fmt.Sprintf("kubectl is pointed at %q but this config is for %q", name, cfg.ClusterName())
	case region != cfg.Cloud.Region:
		return fmt.Sprintf("kubectl is pointed at %s in region %s, but this config is for region %s",
			name, region, cfg.Cloud.Region)
	case account != cfg.Cloud.AccountID:
		return fmt.Sprintf("kubectl is pointed at %s in account %s, but this config is for account %s",
			name, account, cfg.Cloud.AccountID)
	}
	return ""
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
