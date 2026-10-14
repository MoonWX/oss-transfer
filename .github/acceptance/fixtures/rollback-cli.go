//go:build ignore

// This CI fixture gives the candidate CLI different bytes while preserving its behavior.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func main() {
	delegate := os.Getenv("QA_REAL_CLI")
	if !filepath.IsAbs(delegate) {
		fmt.Fprintln(os.Stderr, "QA_REAL_CLI must be an absolute executable path")
		os.Exit(1)
	}
	command := exec.Command(delegate, os.Args[1:]...)
	command.Stdout, command.Stderr, command.Stdin = os.Stdout, os.Stderr, os.Stdin
	if err := command.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
