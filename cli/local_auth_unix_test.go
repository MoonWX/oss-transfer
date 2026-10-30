//go:build !windows

package main

import (
	"os"
	"testing"
)

func TestLocalAuthUnreadablePrivateFileAllowsAccountFallback(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read mode 000 files")
	}
	_, executable := discoveryTestPaths(t)
	origin := "http://127.0.0.1:1685"
	writeTestConnection(t, executable, "master", origin)
	path := executable + ".local-auth.json"
	writeTestLocalAuth(t, path, "master", origin)
	if err := os.Chmod(path, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0600) })
	if token, err := loadLocalCLIAuth(origin); err != nil || token != "" {
		t.Fatal("unreadable system credential blocked account fallback", err)
	}
}
