// Package engine runs the ordered bootstrap pipeline that takes an operator
// from zero to a running nanohype platform.
package engine

import (
	"context"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/exec"
)

// Repos holds the local paths of the nanohype repos rackctl orchestrates.
type Repos struct {
	Workdir       string // base dir, e.g. ~/.rackctl/<org>
	LandingZone   string // landing-zone (Terragrunt substrate)
	EKSGitops     string // the org's fork of nanohype/eks-gitops (addon catalog)
	AgentPlatform string // eks-agent-platform (operator + CRDs + charts)
	Portal        string // portal (day-2 UI; cloned only when controlPlane.portal)
	EKSFleet      string // eks-fleet (cluster control plane; cloned only when controlPlane.eksFleet)
}

// State threads shared data through the phase pipeline.
type State struct {
	Config  *config.Config
	Runner  *exec.Runner
	Repos   Repos
	Outputs map[string]string // captured terragrunt/aws outputs (IRSA ARNs, bucket names, ...)

	// KubeconfigCluster is the cluster the ambient kubeconfig points at, set by the
	// cluster phase once `aws eks update-kubeconfig` has succeeded — and empty until
	// then, which is the fact the rollback's reap sweep is gated on.
	//
	// Everything that sweep does is aimed at whatever context kubectl currently
	// resolves, and rackctl repoints that in exactly one place. Before it runs, the
	// kubeconfig still belongs to whatever the operator was doing beforehand, so
	// "did this run build a cluster?" and "is it safe to delete every Platform and PVC
	// kubectl can see?" are the same question. Recording it here rather than inferring
	// it from the completed-phase list is deliberate: a cluster phase that fails AFTER
	// update-kubeconfig has still repointed the kubeconfig and still has a real cluster
	// to reap, and that is precisely the case a rollback exists for.
	KubeconfigCluster string
}

// Phase is one ordered step of the 0→running bootstrap.
type Phase interface {
	ID() string
	Title() string
	// Optional phases run only when Enabled reports true.
	Optional() bool
	Enabled(*State) bool
	Run(context.Context, *State) error
	// Teardown reverses this phase's cloud writes; called in reverse order on failure.
	Teardown(context.Context, *State) error
}
