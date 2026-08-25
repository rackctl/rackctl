package main

import (
	"fmt"
	"os"

	"github.com/rackctl/rackctl/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		// The status is named as well as returned. An operator reading a terminal and an
		// agent reading $? then have the same vocabulary for what happened, rather than
		// one seeing prose and the other seeing 1 for every distinct outcome.
		fmt.Fprintf(os.Stderr, "%s: %v\n", cmd.Classify(err), err)
		os.Exit(cmd.ExitCode(err))
	}
}
