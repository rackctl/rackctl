package engine

import (
	"os"
	"path/filepath"
)

// RepoPaths returns the standard local layout for an org's cloned platform
// repos, under ~/.rackctl/<org>.
func RepoPaths(org string) Repos {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	work := filepath.Join(home, ".rackctl", org)
	return Repos{
		Workdir:       work,
		LandingZone:   filepath.Join(work, "landing-zone"),
		AgentPlatform: filepath.Join(work, "eks-agent-platform"),
		EKSGitops:     filepath.Join(work, "eks-gitops"),
	}
}
