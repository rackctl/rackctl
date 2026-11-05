package reap

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"

	"github.com/rackctl/rackctl/internal/exec"
	"github.com/rackctl/rackctl/internal/ui"
)

// Outcome is what one backstop did, which is a different question from whether the teardown
// around it succeeded.
//
// Every sweep here guards something terraform cannot see. A teardown whose components all
// destroyed cleanly can still leave a Karpenter node billing, an operator-minted role, or a
// PVC whose volume was never released, and the only record of that used to be a warning
// line scrolled past on the way to "platform destroyed". The outcome makes the record a
// value, so the end of a teardown can say which guards actually held.
type Outcome int

const (
	// FoundNothing means the backstop ran and selected nothing.
	FoundNothing Outcome = iota
	// Acted means the backstop ran and dealt with everything it selected — or, in a dry-run,
	// enumerated what it would deal with.
	Acted
	// LeftBehind means the backstop ran and deliberately left resources it could not prove
	// belong to this run. Nothing failed; something remains, named.
	LeftBehind
	// Failed means the backstop ran and an action it attempted did not succeed.
	Failed
	// NotRun means the backstop did not run: its enumeration failed, or what it acts on could
	// not be reached.
	NotRun
)

func (o Outcome) String() string {
	switch o {
	case FoundNothing:
		return "found nothing"
	case Acted:
		return "ran"
	case LeftBehind:
		return "left resources it could not prove were this run's"
	case Failed:
		return "FAILED"
	default:
		return "did NOT run"
	}
}

// Backstop is one sweep's account of itself.
type Backstop struct {
	Name    string
	Outcome Outcome
	Detail  string
	// Informational marks a backstop that only reports — it deletes nothing, so what it
	// finds or misses never decides whether the teardown held. Its line still prints.
	Informational bool
}

// Clean reports whether every backstop that guards something ran without failing. A
// backstop that did not run is not clean: "nothing was found" and "nothing was looked for"
// are opposite facts about what is still billing.
func Clean(bs []Backstop) bool {
	return len(Unheld(bs)) == 0
}

// Unheld names every backstop that guards something and failed or did not run, in order.
// Informational backstops are never named: they delete nothing, so what they miss cannot
// leave anything billing.
func Unheld(bs []Backstop) []string {
	var names []string
	for _, b := range bs {
		if b.Informational {
			continue
		}
		if b.Outcome == Failed || b.Outcome == NotRun {
			names = append(names, b.Name)
		}
	}
	return names
}

// ClusterGone reports whether EKS says the cluster does not exist. Only
// ResourceNotFoundException answers yes; any other failure leaves the question open, and
// an open question is not a cluster that is gone.
//
// A destroy that could not point kubectl at its cluster asks this, because the idempotent
// re-run after a partial teardown reaches a cluster that the first run already deleted, and
// a cluster that is gone has nothing in it to reap.
func ClusterGone(ctx context.Context, run *exec.Runner, cluster, region string) bool {
	return clusterGone(ctx, run, cluster, region)
}

func clusterGone(ctx context.Context, run execer, cluster, region string) bool {
	if cluster == "" {
		return false
	}
	args := []string{"eks", "describe-cluster", "--name", cluster}
	if region != "" {
		args = append(args, "--region", region)
	}
	_, err := run.Query(ctx, "aws", append(args, "--query", "cluster.status", "--output", "text")...)
	return err != nil && strings.Contains(err.Error(), "ResourceNotFoundException")
}

// Summarize prints the backstop summary — one line per backstop, saying which ran, found
// nothing, failed or did not run — and reports whether it was clean.
func Summarize(out io.Writer, bs []Backstop) bool {
	if len(bs) == 0 {
		return true
	}
	fmt.Fprintln(out, ui.Step("backstop summary"))
	for _, b := range bs {
		line := b.Name + ": " + b.Outcome.String()
		if b.Detail != "" {
			line += " — " + b.Detail
		}
		switch b.Outcome {
		case FoundNothing, Acted:
			fmt.Fprintln(out, "  "+ui.OK(line))
		case Failed:
			fmt.Fprintln(out, "  "+ui.Fail(line))
		default:
			fmt.Fprintln(out, "  "+ui.Warn(line))
		}
	}
	return Clean(bs)
}

// EgressIP resolves this host's public egress IP. The caller wires it to the same resolver
// the cluster phase uses to scope the endpoint allow-list, so an unreachable cluster can be
// reported with the address its allow-list would have to hold. Nil leaves it unreported.
var EgressIP func(context.Context) (string, error)

// unreachable is the in-cluster reap's account of a cluster whose API did not answer.
//
// After a successful repoint, a readyz failure does not mean there is no cluster: the
// repoint itself described the cluster to write the kubeconfig. It means the cluster exists
// and this host cannot reach its API — a private endpoint, or an allow-list pinned to another
// egress. "No reachable cluster — nothing to reap" said the opposite of that, and the
// teardown then destroyed the cluster on top of the Platforms, NodeClaims and PVCs the reap
// exists to remove first.
//
// So it asks EKS directly. ResourceNotFoundException is a cluster that is really gone, and
// there is nothing to reap. Anything else is a reap that did not run, said with the endpoint
// posture and the egress address that would explain it.
func unreachable(ctx context.Context, run execer, out io.Writer, cluster, region string, probe error) Backstop {
	const name = "in-cluster reap"
	notRun := func(detail string) Backstop {
		fmt.Fprintln(out, ui.Warn("in-cluster reap did NOT run — "+detail))
		return Backstop{Name: name, Outcome: NotRun, Detail: detail}
	}
	if cluster == "" {
		return notRun(fmt.Sprintf("the cluster API did not answer (%v), and there is no cluster name to ask EKS about", probe))
	}

	raw, err := run.Query(ctx, "aws", "eks", "describe-cluster", "--name", cluster, "--region", region,
		"--query", "cluster.{status:status,public:resourcesVpcConfig.endpointPublicAccess,"+
			"private:resourcesVpcConfig.endpointPrivateAccess,cidrs:resourcesVpcConfig.publicAccessCidrs}",
		"--output", "json")
	if err != nil {
		if strings.Contains(err.Error(), "ResourceNotFoundException") {
			fmt.Fprintln(out, ui.Step("no cluster named "+cluster+" — nothing to reap"))
			return Backstop{Name: name, Outcome: FoundNothing, Detail: "the cluster no longer exists"}
		}
		return notRun(fmt.Sprintf("the %s API did not answer (%v), and describe-cluster failed too (%v), "+
			"so whether there is anything to reap is unknown", cluster, probe, err))
	}
	var desc struct {
		Status  string   `json:"status"`
		Public  bool     `json:"public"`
		Private bool     `json:"private"`
		CIDRs   []string `json:"cidrs"`
	}
	if err := json.Unmarshal([]byte(raw), &desc); err != nil {
		return notRun(fmt.Sprintf("the %s API did not answer (%v), and describe-cluster's answer could not "+
			"be read (%v)", cluster, probe, err))
	}

	posture := fmt.Sprintf("endpoint public=%t private=%t", desc.Public, desc.Private)
	if desc.Public {
		posture += ", allow-list " + strings.Join(desc.CIDRs, ", ")
		posture += egressAgainst(ctx, desc.CIDRs)
	}
	return notRun(fmt.Sprintf(
		"%s exists (%s) but its API does not answer from this host (%v); %s. Its Platforms, Tenants, "+
			"NodeClaims and PVCs were not reaped, so operator-minted IAM roles may stop agent-iam on "+
			"DeleteConflict and EBS volumes the CSI driver never released may outlive the cluster; the IAM "+
			"and EC2 "+
			"backstops still run. Re-run from a host the endpoint admits to reap them",
		cluster, desc.Status, probe, posture))
}

// egressAgainst names this host's egress IP and whether the allow-list admits it.
func egressAgainst(ctx context.Context, cidrs []string) string {
	if EgressIP == nil {
		return ""
	}
	ip, err := EgressIP(ctx)
	if err != nil {
		return fmt.Sprintf("; this host's egress IP could not be detected (%v)", err)
	}
	addr := net.ParseIP(ip)
	for _, c := range cidrs {
		if _, n, err := net.ParseCIDR(c); err == nil && addr != nil && n.Contains(addr) {
			return "; this host's egress IP " + ip + " is inside it, so the block is elsewhere"
		}
	}
	return "; this host's egress IP " + ip + " is NOT in it"
}
