//go:build !windows

package transferstore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTempNamespaceUnixRejectsSharedPermissions(t *testing.T) {
	path, parent, record := makeTestNamespace(t)
	if err := os.Chmod(filepath.Join(path, record.Name), 0770); err != nil {
		t.Fatal(err)
	}
	if root, err := OpenLocalTempNamespace(parent, record); err == nil {
		root.Close()
		t.Fatal("namespace writable by other users was accepted")
	}
}
