// Package gitops implements the file-level rewrites rackctl performs against the
// operator's eks-gitops fork — the account-id writeback into the addon values files.
//
// The upstream catalog commits no account id at all: it is public, addons bind their
// IAM roles through EKS Pod Identity (which needs no ARN in the values), and the one
// ApplicationSet that does need a role ARN templates it from an annotation
// cluster-bootstrap stamps on the ArgoCD cluster Secret. So against upstream this
// writeback substitutes nothing, and the substrate phase says so rather than reporting
// a bare zero. It rewrites whatever DOES carry a placeholder, which is what makes a
// fork the org has taken ownership of — the entire reason rackctl forks the catalog —
// safe to hand-edit with a literal account id.
package gitops

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Placeholder is the dummy account id an addon values file uses to stand in for the
// real one (e.g. arn:aws:iam::000000000000:role/...). rackctl replaces it with the
// account id after the cluster-addons apply, so the catalog ArgoCD clones in the next
// phase already resolves to this account.
const Placeholder = "000000000000"

// SubstituteAccountID replaces every Placeholder occurrence in VALUES with accountID and
// returns the rewritten content plus the number of replacements.
//
// Comments are left alone, and this is a write path so that matters twice over.
//
// The account id must never be committed to the public catalog — that is the entire reason
// the placeholder exists. Substituting it inside a comment writes the real id into the
// operator's fork in a place nothing reads and nobody thinks to look, which is a leak
// rather than a rewrite. And the count is reported to the operator and decides whether the
// file is written at all, so counting a comment hit means a file whose only placeholder is
// commentary is rewritten and reported as substituted while nothing functional changed.
//
// Line by line with the comment split found quote-aware, rather than blanking and
// replacing: the comments have to survive verbatim in the output, so the locator and the
// writer must agree on the same boundary.
func SubstituteAccountID(content, accountID string) (string, int) {
	if !strings.Contains(content, Placeholder) {
		return content, 0
	}
	lines := strings.Split(content, "\n")
	var n int
	for i, line := range lines {
		value, comment := splitComment(line)
		c := strings.Count(value, Placeholder)
		if c == 0 {
			continue
		}
		n += c
		lines[i] = strings.ReplaceAll(value, Placeholder, accountID) + comment
	}
	if n == 0 {
		return content, 0
	}
	return strings.Join(lines, "\n"), n
}

// splitComment divides a YAML line into its value and its trailing comment, respecting
// quotes so a '#' inside a quoted string is not mistaken for one.
func splitComment(line string) (value, comment string) {
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t'):
			return line[:i], line[i:]
		}
	}
	return line, ""
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
