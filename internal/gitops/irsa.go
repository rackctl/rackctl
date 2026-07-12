// Package gitops implements the file-level rewrites rackctl performs against the
// operator's eks-gitops fork — most importantly the IRSA account-id writeback,
// the single biggest manual footgun in the current install flow.
package gitops

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Placeholder is the dummy account id that landing-zone leaves in the eks-gitops
// addon values files (e.g. arn:aws:iam::000000000000:role/...). rackctl replaces
// it with the real account id after the cluster-addons apply.
const Placeholder = "000000000000"

// SubstituteAccountID replaces every Placeholder occurrence with accountID and
// returns the rewritten content plus the number of replacements.
func SubstituteAccountID(content, accountID string) (string, int) {
	n := strings.Count(content, Placeholder)
	if n == 0 {
		return content, 0
	}
	return strings.ReplaceAll(content, Placeholder, accountID), n
}

// WriteBack rewrites every values-<env>.yaml under <gitopsDir>/addons/**,
// substituting the account-id placeholder. It returns the total number of
// replacements and the list of files it changed (for staging by name).
func WriteBack(gitopsDir, env, accountID string) (int, []string, error) {
	root := filepath.Join(gitopsDir, "addons")
	target := "values-" + env + ".yaml"

	var total int
	var changed []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() != target {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out, n := SubstituteAccountID(string(b), accountID)
		if n == 0 {
			return nil
		}
		if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
			return err
		}
		total += n
		changed = append(changed, path)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return total, changed, nil
}
