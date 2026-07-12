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
}

// State threads shared data through the phase pipeline.
type State struct {
	Config  *config.Config
	Runner  *exec.Runner
	Repos   Repos
	Outputs map[string]string // captured terragrunt/aws outputs (IRSA ARNs, bucket names, ...)
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
