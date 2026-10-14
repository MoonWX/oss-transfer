//go:build ignore

// This CI fixture passes real preflight operations, then fails service startup.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func main() {
	for _, argument := range os.Args[1:] {
		switch argument {
		case "--version", "--check-config", "--backup-state", "--restore-state":
			delegate := os.Getenv("QA_REAL_MASTER")
			if !filepath.IsAbs(delegate) {
				fmt.Fprintln(os.Stderr, "QA_REAL_MASTER must be an absolute executable path")
				os.Exit(1)
			}
			command := exec.Command(delegate, os.Args[1:]...)
			command.Stdout, command.Stderr, command.Stdin = os.Stdout, os.Stderr, os.Stdin
			if err := command.Run(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			return
		}
	}
	fmt.Fprintln(os.Stderr, "injected startup failure")
	os.Exit(17)
}
