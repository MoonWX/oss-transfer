//go:build !windows

package main

import (
	"os"
	"testing"
)

func testSessionPermissions(t *testing.T, path, host string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("session mode: %v %v", info, err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSession(host); err == nil {
		t.Fatal("unsafe session permissions accepted")
	}
	if err := protectSessionPath(path, false); err != nil {
		t.Fatal(err)
	}
}
