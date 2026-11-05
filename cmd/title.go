package cmd

import (
	"fmt"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/ui"
)

// commandTitle is the banner a command prints before it acts.
//
// It carries the ACCOUNT and the PROFILE because the destroy runbook tells
// operators to "confirm the account, region, and profile in the printed title
// before you run it" — and the title contained neither of the two that decide
// which cloud is about to be changed. An operator following that instruction
// literally was confirming an org name and a region, and a region is shared by
// every account they have.
//
// Built in one place so the three commands cannot drift apart. They were three
// separate Sprintf calls, which is how they came to be identical in shape and
// therefore equally wrong.
func commandTitle(verb string, cfg *config.Config) string {
	return fmt.Sprintf("rackctl %s — %s · %s · %s · %s · %s",
		verb,
		cfg.Org.Name,
		cfg.Cloud.AccountID,
		cfg.Cloud.Profile,
		cfg.Cloud.Region,
		cfg.Environment,
	)
}

// defaultedLines is the block a command prints under its title naming every value the config
// left out and rackctl filled in. A default is a decision nobody wrote down; some decide whether
// the EKS API faces the internet or what sizes the node group gets, so each one is said rather
// than left to be discovered. Nothing when the config names every value.
func defaultedLines(cfg *config.Config) []string {
	d := cfg.Defaulted()
	if len(d) == 0 {
		return nil
	}
	lines := []string{ui.Step(fmt.Sprintf("%d value(s) not set in the config, using defaults (set them to choose):", len(d)))}
	for _, v := range d {
		lines = append(lines, fmt.Sprintf("    %s: %s", v.Field, v.Value))
	}
	return lines
}
