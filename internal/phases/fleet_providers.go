package phases

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/rackctl/rackctl/internal/engine"
)

// fleetRoleARNLine matches the ServiceAccount annotation eks-fleet ships as a placeholder.
//
// [ \t]* rather than \s*: under (?m), `^` matches at every line start and \s matches a
// newline, so `^(\s*)` starts the match on a blank line ABOVE the annotation and reports a
// position that is not the annotation's. The rewrite happens to survive that because ${1}
// re-emits the whitespace it swallowed — so this is a locator defect with no output
// corruption today, and exactly the shape that becomes one the moment a caller reports the
// position or the replacement stops re-emitting the capture.
//
// The anchor also keeps a COMMENTED annotation out: `# eks.amazonaws.com/role-arn:` has a
// '#' where the pattern requires the key, so a superseded line above the live one cannot
// win the match.
var fleetRoleARNLine = regexp.MustCompile(`(?m)^([ \t]*)eks\.amazonaws\.com/role-arn:.*$`)

// renderFleetProviders writes a copy of eks-fleet's config/bootstrap/providers.yaml with
// the real hub role ARN substituted, and returns the path to apply.
//
// The file in the repo carries a LITERAL placeholder:
//
//	eks.amazonaws.com/role-arn: arn:aws:iam::<FLEET_ACCOUNT_ID>:role/eks-fleet-crossplane
//
// and eks-fleet's own stand-up-the-hub.md says so explicitly — "the file stays a
// placeholder" — piping it through sed before kubectl. Applying it as shipped installs
// provider-opentofu with a ServiceAccount annotation that is not an ARN, so EKS never
// hands the pod credentials; the provider still reports Healthy (installation succeeded),
// so the phase declares success and every spoke vend fails later with no obvious cause.
//
// It is also actively destructive of a correct setup. kubectl apply is declarative, so an
// operator who has put the real ARN on the ServiceAccount gets it reverted to the
// placeholder — and a note telling them to check that annotation, printed immediately
// before reverting it, is worse than saying nothing.
//
// rackctl supplies fragile per-run inputs; this is one. The ARN is normally COMPUTED
// rather than configured: the substrate phase applies landing-zone's fleet-hub into this
// account, and that component pins the role name so eks-fleet's committed annotation can
// match it. controlPlane.fleetHubRoleArn overrides, for a hub in another account.
//
// The rendered copy is written OUTSIDE the eks-fleet checkout, to a temporary file the
// returned cleanup removes, and applied by absolute path. A file written into the checkout
// is an untracked file there: `git checkout .` does not remove it, and whether it collides
// with anything depends on upstream's .gitignore, which is not rackctl's to rely on.
//
// The annotation is matched by its key, and exactly one is substituted. More than one
// means the file now wires more than one identity, and rackctl will not guess which ARN
// each should carry.
func renderFleetProviders(st *engine.State) (string, func(), error) {
	const src = "config/bootstrap/providers.yaml"
	arn := st.Config.FleetHubRoleARN()
	none := func() {}

	note(st, "rendering %s with eks.amazonaws.com/role-arn=%s — the file ships a literal "+
		"<FLEET_ACCOUNT_ID> placeholder, and applying it unrendered leaves provider-opentofu "+
		"with no credentials while still reporting Healthy", src, arn)

	if st.Runner.DryRun {
		return src, none, nil
	}

	full := filepath.Join(st.Repos.EKSFleet, src)
	b, err := os.ReadFile(full)
	if err != nil {
		return "", none, fmt.Errorf("reading %s from the eks-fleet checkout: %w", src, err)
	}
	body := string(b)

	// Zero matches means the file's shape changed upstream — fail rather than apply a
	// placeholder silently.
	switch n := len(fleetRoleARNLine.FindAllStringIndex(body, -1)); {
	case n == 0:
		return "", none, &engine.NoRollbackError{Err: fmt.Errorf(
			"%s no longer carries an eks.amazonaws.com/role-arn annotation. rackctl substitutes the "+
				"hub role ARN there because the file ships a <FLEET_ACCOUNT_ID> placeholder; if upstream "+
				"changed how provider-opentofu gets its identity, this phase needs updating rather than "+
				"applying the file blind. See eks-fleet/docs/stand-up-the-hub.md", src)}
	case n > 1:
		return "", none, &engine.NoRollbackError{Err: fmt.Errorf(
			"%s carries %d eks.amazonaws.com/role-arn annotations, and rackctl substitutes exactly one. "+
				"The file now wires more than one identity, and writing the hub role into every one of "+
				"them would hand it to whatever the others are; this phase needs updating for the new "+
				"shape. See eks-fleet/docs/stand-up-the-hub.md", src, n)}
	}
	out := fleetRoleARNLine.ReplaceAllString(body, "${1}eks.amazonaws.com/role-arn: "+arn)
	note(st, "substituted the hub role into the one eks.amazonaws.com/role-arn annotation in %s", src)

	// Belt and braces: if the placeholder token survived, something did not match the way
	// this expects, and a provider with an unresolvable annotation is worse than a failure.
	if strings.Contains(out, "<FLEET_ACCOUNT_ID>") {
		return "", none, &engine.NoRollbackError{Err: fmt.Errorf(
			"%s still contains <FLEET_ACCOUNT_ID> after substitution — refusing to install a provider "+
				"that cannot receive credentials", src)}
	}

	f, err := os.CreateTemp("", "rackctl-eks-fleet-providers-*.yaml")
	if err != nil {
		return "", none, fmt.Errorf("creating a file for the rendered %s: %w", src, err)
	}
	cleanup := func() { _ = os.Remove(f.Name()) }
	_, werr := f.WriteString(out)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		cleanup()
		return "", none, fmt.Errorf("writing the rendered %s: %w", src, werr)
	}
	return f.Name(), cleanup, nil
}

// verifyFleetHubRole asks IAM whether the hub role the annotation will carry exists, before
// anything is installed on its behalf. provider-opentofu reports Healthy with a role that
// does not exist, so nothing downstream would say so.
//
// Only a role in this account can be read from here. A configured override in another
// account is reported as unverified rather than checked, and a read that did not answer is
// reported the same way: neither is evidence the role is missing. A plan reads for real and
// only reports, because on a first install the substrate phase has not yet created the role.
func verifyFleetHubRole(ctx context.Context, st *engine.State) error {
	arn := st.Config.FleetHubRoleARN()
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) < 6 {
		return fmt.Errorf("controlPlane.fleetHubRoleArn %q is not an IAM role ARN", arn)
	}
	if acct := parts[4]; acct != st.Config.Cloud.AccountID {
		note(st, "hub role %s is in account %s, not this one, so rackctl cannot read it from here: "+
			"it is UNVERIFIED. A role that does not exist installs a provider that reports Healthy "+
			"and never receives credentials", arn, acct)
		return nil
	}
	name := arn[strings.LastIndex(arn, "/")+1:]
	got, err := st.Runner.Query(ctx, "aws", "iam", "get-role", "--role-name", name,
		"--query", "Role.Arn", "--output", "text")
	switch {
	case err != nil && strings.Contains(err.Error(), "NoSuchEntity") && st.Runner.DryRun:
		note(st, "hub role %s does not exist yet; (apply) the substrate phase's fleet-hub creates it, "+
			"and this phase checks for it again before installing anything", arn)
		return nil
	case err != nil && strings.Contains(err.Error(), "NoSuchEntity"):
		return fmt.Errorf("aws iam: the hub role %s does not exist in this account. provider-opentofu "+
			"would install, report Healthy and never receive credentials, so nothing was installed. "+
			"The fleet-hub component creates it; check that component's apply, or set "+
			"controlPlane.fleetHubRoleArn to the role that exists", arn)
	case err != nil:
		note(st, "could not read hub role %s (%v): it is UNVERIFIED for this run", arn, err)
		return nil
	case got != arn:
		return fmt.Errorf("aws iam: the role named %s is %s, not %s — the annotation would carry an "+
			"ARN that EKS cannot resolve, so nothing was installed. Set controlPlane.fleetHubRoleArn "+
			"to the role's own ARN", name, got, arn)
	}
	note(st, "hub role %s exists in this account", arn)
	return nil
}
