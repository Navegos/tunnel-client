package main

import (
	"fmt"
	"os"
)

func main() {
	rootCmd := newRootCommand(os.LookupEnv, os.Stdout, os.Stderr)
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
