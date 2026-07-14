// Package cmd wires the rackctl CLI.
package cmd

import "github.com/spf13/cobra"

var rootCmd = &cobra.Command{
	Use:   "rackctl",
	Short: "The day-0 installer for a nanohype platform",
	Long: "rackctl provisions a full nanohype platform from zero — cloud, cluster,\n" +
		"GitOps, controllers, and portal — then hands off to the portal for day-2 ops.",
	SilenceUsage:  true,
	SilenceErrors: true,
}

// Execute runs the root command.
func Execute() error { return rootCmd.Execute() }

func init() {
	rootCmd.AddCommand(initCmd, preflightCmd, doctorCmd, upgradeCmd, destroyCmd, versionCmd)
}
