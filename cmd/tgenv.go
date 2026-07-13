package cmd

import (
	"github.com/rackctl/rackctl/internal/config"
)

// tgEnv builds the environment every terragrunt invocation runs with.
//
// landing-zone's root.hcl resolves the account via TERRAGRUNT_ACCOUNT_ID (its
// account.hcl is a placeholder) and the tfstate bucket is
// {account}-{region}-tfstate, so terragrunt must see the real account.
//
// TF_VAR_gitops_repo_url is the one that matters most, and its absence was a real
// bug: cluster-bootstrap's gitops_repo_url used to default to the UPSTREAM catalog
// (nanohype/eks-gitops), and rackctl never passed a value — it only printed the
// fork's name in a log line. So every install wired its app-of-apps to upstream
// main, unpinned, while the fork rackctl had just created for the org sat unread.
// A cluster vended into another org was found syncing from nanohype/eks-gitops@main.
//
// landing-zone now declares gitops_repo_url with NO default, so a missing value
// fails the plan instead of silently borrowing someone else's catalog. Passing it
// here is what satisfies that. Terragrunt/tofu pick up TF_VAR_* automatically, so
// this needs no terragrunt.hcl inputs block and stays correct for every component.
func tgEnv(cfg *config.Config) []string {
	env := []string{
		"AWS_PROFILE=" + cfg.Cloud.Profile,
		"AWS_REGION=" + cfg.Cloud.Region,
		"TERRAGRUNT_ACCOUNT_ID=" + cfg.Cloud.AccountID,
	}
	if u := cfg.Org.GitOps.GitURL(); u != "" {
		env = append(env, "TF_VAR_gitops_repo_url="+u)
	}
	return env
}
