//go:build !windows

package agentservice

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/state"
)

func fileIdentityForTest(info fs.FileInfo) string {
	st := info.Sys().(*syscall.Stat_t)
	return fmt.Sprintf("/%d/%d", st.Dev, st.Ino)
}

// A crash after deferred writes published their objects but before the
// barrier leaves registered temporary names, possibly resurrected because
// their removal was not durable. Recovery removes them and the namespace, and
// keeps the published objects.
func TestLocalTempRecoveryAfterCrashBeforeBarrier(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	root := t.TempDir()
	db, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewLocalTempRegistry(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	local := trackedLocal(t, registry, root)
	deferredWrite(t, local, "out/linked", "published by link", false)
	deferredWrite(t, local, "out/renamed", "published by rename", true)
	// Two written plus reserved, still empty temporary files.
	records := tempRecords(t, db)
	if len(records) < 2 {
		t.Fatalf("deferred writes must stay registered until the barrier: %+v", records)
	}
	// Simulate lost directory updates: both temporary names come back as
	// links to the published files, as an unsynced unlink or rename would.
	for _, record := range records {
		for _, key := range []string{"out/linked", "out/renamed"} {
			published := filepath.Join(root, key)
			info, err := os.Stat(published)
			if err != nil {
				t.Fatal(err)
			}
			if record.FileIdentity == fileIdentityForTest(info) {
				if err := os.Link(published, filepath.Join(record.ParentPath, record.Name)); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	// Crash: the Local is never synced or closed.
	registry.Close()
	db.Close()

	restarted, restartedDB := openTempRegistry(t, path)
	report, err := restarted.Recover(context.Background())
	if err != nil || report.Removed != len(records) || report.RemovedNamespaces != 1 || len(report.Retained) != 0 {
		t.Fatalf("recovery after crash before barrier: %+v %v", report, err)
	}
	if len(tempRecords(t, restartedDB)) != 0 || len(namespaceRecords(t, restartedDB)) != 0 || len(stagingDirectories(t, filepath.Join(root, "out"))) != 0 {
		t.Fatal("recovery left temporary state behind")
	}
	for key, want := range map[string]string{"out/linked": "published by link", "out/renamed": "published by rename"} {
		if body, err := os.ReadFile(filepath.Join(root, key)); err != nil || string(body) != want {
			t.Fatalf("recovery damaged %s: %q %v", key, body, err)
		}
	}
}
