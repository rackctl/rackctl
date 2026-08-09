package engine

import (
	"os"
	"path/filepath"
	"strings"
)

// UpstreamCatalog is the canonical addon catalog every platform's ArgoCD ultimately
// derives from. An org consumes it by forking it, because the cluster reads the catalog
// from the fork and the org is expected to commit to that fork.
//
// It is a constant in one place rather than a literal at each use because the two users —
// the acquire phase and preflight's fork check — must agree on it exactly. Two literals
// that drift produce a preflight that passes against one repo and a fork of another.
const UpstreamCatalog = "nanohype/eks-gitops"

// UpstreamCatalogOwner is the org that publishes UpstreamCatalog.
//
// It is derived rather than declared: an owner constant sitting beside the repo constant
// is a second thing to keep in step, and the only failure it can produce is the silent
// one where they disagree.
func UpstreamCatalogOwner() string {
	owner, _, _ := strings.Cut(UpstreamCatalog, "/")
	return owner
}

// OwnsUpstreamCatalog reports whether org publishes the upstream catalog itself, in which
// case there is no fork in the picture: the org's catalog IS upstream.
//
// This is not a hypothetical. nanohype installs its own platform from its own catalog, and
// the forking code paths are wrong for it in both directions — `gh repo fork` cannot fork a
// repo into the account that owns it, and `gh repo sync X --source X` asks GitHub to
// fast-forward a branch onto itself.
func OwnsUpstreamCatalog(org string) bool {
	return org == UpstreamCatalogOwner()
}

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
		Portal:        filepath.Join(work, "portal"),
		EKSFleet:      filepath.Join(work, "eks-fleet"),
	}
}
