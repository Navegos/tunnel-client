package main

import (
	"fmt"
	"os"
)

func main() {
	rootCmd := newRootCommand(os.LookupEnv, os.Stdout, os.Stderr)
	if err := rootCmd.Execute(); err != nil {
		exitCode := 1
		if coded, ok := err.(interface{ ExitCode() int }); ok {
			exitCode = coded.ExitCode()
		}
		if err.Error() != "" {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(exitCode)
	}
}
