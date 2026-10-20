package main

import (
	"errors"
	"os"
	"path/filepath"
)

func defaultAgentDataDirectory() (string, error) {
	const current = "./data/agent"
	const legacy = "./agent-data"
	found := make(map[string]bool, 2)
	for _, directory := range []string{current, legacy} {
		info, err := os.Stat(filepath.Join(directory, "agent.db"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() {
			return "", errors.New("Agent database is not a regular file; choose --data-dir")
		}
		found[directory] = true
	}
	if found[current] && found[legacy] {
		return "", errors.New("Agent databases exist in ./data/agent and ./agent-data; choose --data-dir")
	}
	if found[legacy] {
		return legacy, nil
	}
	return current, nil
}
