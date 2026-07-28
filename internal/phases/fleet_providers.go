package phases

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/rackctl/rackctl/internal/engine"
)

// fleetRoleARNLine matches the ServiceAccount annotation eks-fleet ships as a placeholder.
var fleetRoleARNLine = regexp.MustCompile(`(?m)^(\s*)eks\.amazonaws\.com/role-arn:.*$`)

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
// operator who had put the real ARN on the ServiceAccount gets it reverted to the
// placeholder — which is what rackctl used to do immediately after printing a note telling
// them to make sure that annotation was right.
//
// rackctl supplies fragile per-run inputs; this is one. The ARN comes from config because
// fleet-hub lives under live/aws/fleet/…, which rackctl does not apply and cannot capture
// an output from.
func renderFleetProviders(st *engine.State) (string, error) {
	const src = "config/bootstrap/providers.yaml"
	arn := st.Config.ControlPlane.FleetHubRoleARN

	note(st, "rendering %s with eks.amazonaws.com/role-arn=%s — the file ships a literal "+
		"<FLEET_ACCOUNT_ID> placeholder, and applying it unrendered leaves provider-opentofu "+
		"with no credentials while still reporting Healthy", src, arn)

	if st.Runner.DryRun {
		return src, nil
	}

	full := filepath.Join(st.Repos.EKSFleet, src)
	b, err := os.ReadFile(full)
	if err != nil {
		return "", fmt.Errorf("reading %s from the eks-fleet checkout: %w", src, err)
	}
	body := string(b)

	// Substitute every occurrence, preserving each line's indentation. Zero matches means
	// the file's shape changed upstream — fail rather than apply a placeholder silently.
	if !fleetRoleARNLine.MatchString(body) {
		return "", &engine.NoRollbackError{Err: fmt.Errorf(
			"%s no longer carries an eks.amazonaws.com/role-arn annotation. rackctl substitutes the "+
				"hub role ARN there because the file ships a <FLEET_ACCOUNT_ID> placeholder; if upstream "+
				"changed how provider-opentofu gets its identity, this phase needs updating rather than "+
				"applying the file blind. See eks-fleet/docs/stand-up-the-hub.md", src)}
	}
	out := fleetRoleARNLine.ReplaceAllString(body, "${1}eks.amazonaws.com/role-arn: "+arn)

	// Belt and braces: if the placeholder token survived, something did not match the way
	// this expects, and a provider with an unresolvable annotation is worse than a failure.
	if strings.Contains(out, "<FLEET_ACCOUNT_ID>") {
		return "", &engine.NoRollbackError{Err: fmt.Errorf(
			"%s still contains <FLEET_ACCOUNT_ID> after substitution — refusing to install a provider "+
				"that cannot receive credentials", src)}
	}

	dst := filepath.Join(st.Repos.EKSFleet, ".rackctl-providers.yaml")
	if err := os.WriteFile(dst, []byte(out), 0o644); err != nil {
		return "", fmt.Errorf("writing the rendered %s: %w", src, err)
	}
	// Relative, because Runner.Dir is already the eks-fleet checkout.
	return ".rackctl-providers.yaml", nil
}
