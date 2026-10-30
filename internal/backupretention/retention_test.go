package backupretention

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func fixture(t *testing.T) (*Manager, string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "backups")
	identity := Identity{Role: "master", ServiceName: "oss-transfer-master", Config: filepath.Join(base, "config", "service.json"), DataDir: filepath.Join(base, "data")}
	m, err := New(root, identity)
	if err != nil {
		t.Fatal(err)
	}
	m.now = func() time.Time { return time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC) }
	if err = os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	return m, base
}

func write(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

func makeBackup(t *testing.T, m *Manager, name string) {
	t.Helper()
	directory := filepath.Join(m.root, name)
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	config, _ := json.Marshal(map[string]string{"serviceName": m.identity.ServiceName, "dataDirectory": m.identity.DataDir})
	write(t, filepath.Join(directory, "service.json"), string(config))
	write(t, filepath.Join(directory, "database"), "database snapshot contents")
	if runtime.GOOS == "windows" {
		write(t, filepath.Join(directory, "binary.exe"), "old executable")
		metadata, _ := json.Marshal(struct {
			Identity
			Version string `json:"version"`
		}{m.identity, "4.1.18"})
		write(t, filepath.Join(directory, "install.json"), string(metadata))
	} else {
		write(t, filepath.Join(directory, "binary"), "old executable")
		metadata := fmt.Sprintf("role\t%s\nserviceName\t%s\nconfig\t%s\ndataDir\t%s\nversion\t4.1.18\n", m.identity.Role, m.identity.ServiceName, m.identity.Config, m.identity.DataDir)
		write(t, filepath.Join(directory, "install.tsv"), metadata)
	}
}

func completedBackup(t *testing.T, m *Manager, name string, age time.Duration) {
	t.Helper()
	makeBackup(t, m, name)
	when := m.now().Add(-age)
	copy := *m
	copy.now = func() time.Time { return when }
	if err := copy.Complete(name, ""); err != nil {
		t.Fatal(err)
	}
}

func entryNamed(t *testing.T, m *Manager, name string) Entry {
	t.Helper()
	listing, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range listing.Entries {
		if entry.Name == name {
			return entry
		}
	}
	t.Fatalf("entry %q missing in %+v", name, listing)
	return Entry{}
}

func exists(t *testing.T, path string, expected bool) {
	t.Helper()
	_, err := os.Lstat(path)
	if expected && err != nil {
		t.Fatalf("expected %s to exist: %v", path, err)
	}
	if !expected && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected %s removed, got %v", path, err)
	}
}

func TestCompletedUpgradeBackupsAreImmediatelyRemovable(t *testing.T) {
	m, _ := fixture(t)
	completedBackup(t, m, "backup-a", 3*time.Hour)
	completedBackup(t, m, "backup-b", 2*time.Hour)
	completedBackup(t, m, "backup-c", 0)
	listing, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Entries) != 3 || listing.Policy != (Policy{}) || listing.ReclaimableBytes != listing.TotalBytes {
		t.Fatalf("unexpected listing: %+v", listing)
	}
	for _, entry := range listing.Entries {
		if !entry.Eligible || entry.Protected {
			t.Fatalf("completed backup protected: %+v", entry)
		}
	}
	result, err := m.Prune(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(result.Removed, []string{"backup-c", "backup-b", "backup-a"}) || result.FreedBytes != listing.TotalBytes {
		t.Fatalf("unexpected cleanup: %+v", result)
	}
	for _, name := range result.Removed {
		exists(t, filepath.Join(m.root, name), false)
	}
	result, err = m.Prune(Options{})
	if err != nil || len(result.Removed) != 0 || result.FreedBytes != 0 {
		t.Fatalf("repeated cleanup: %+v %v", result, err)
	}
}

func TestProtectsIncompleteFailedLegacyAndActiveBackups(t *testing.T) {
	m, _ := fixture(t)
	completedBackup(t, m, "latest", time.Hour)
	for _, name := range []string{"legacy", "in-progress", "failed", "broken", "invalid-json"} {
		makeBackup(t, m, name)
	}
	failed, _ := json.Marshal(marker{Schema: 1, State: "FAILED", Identity: m.identity, Version: "4.1.18", CompletedAt: m.now().Add(-time.Hour)})
	write(t, filepath.Join(m.root, "failed", MarkerName), string(failed))
	write(t, filepath.Join(m.root, "invalid-json", MarkerName), "{")
	if err := m.Complete("failed", ""); err == nil {
		t.Fatal("failed backup was promoted to successful")
	}
	if err := m.Complete("broken", ""); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(m.root, "broken", "database")); err != nil {
		t.Fatal(err)
	}
	completedBackup(t, m, "active", 60*24*time.Hour)
	write(t, filepath.Join(m.root, "active", ActiveName), "installer still using this backup")
	write(t, filepath.Join(m.root, "unrelated.txt"), "leave this alone")
	result, err := m.Prune(Options{})
	if err != nil || !slices.Equal(result.Removed, []string{"latest"}) {
		t.Fatalf("protected backups removed: %+v %v", result, err)
	}
	for _, name := range []string{"legacy", "in-progress", "failed", "broken", "invalid-json", "active", "unrelated.txt"} {
		entry := entryNamed(t, m, name)
		if !entry.Protected || entry.Eligible {
			t.Fatalf("%s not protected: %+v", name, entry)
		}
		exists(t, filepath.Join(m.root, name), true)
	}
	result, err = m.Prune(Options{Names: []string{"legacy", "failed", "active", "broken"}})
	if err != nil || len(result.Removed) != 0 {
		t.Fatalf("manual selection bypassed protection: %+v %v", result, err)
	}
}

func TestOnlyMatchingInstallationCanManageBackups(t *testing.T) {
	m, base := fixture(t)
	completedBackup(t, m, "our-backup", 3*time.Hour)
	completedBackup(t, m, "our-latest", time.Hour)
	variants := []Identity{m.identity, m.identity, m.identity, m.identity}
	variants[0].Role = "agent"
	variants[1].ServiceName = "second-master"
	variants[2].Config = filepath.Join(base, "other-config", "service.json")
	variants[3].DataDir = filepath.Join(base, "other-data")
	for index, identity := range variants {
		other, err := New(m.root, identity)
		if err != nil {
			t.Fatal(err)
		}
		other.now = m.now
		name := fmt.Sprintf("foreign-%d", index)
		completedBackup(t, other, name, 60*24*time.Hour)
		if entry := entryNamed(t, m, name); entry.Reason != "OTHER_INSTALLATION" {
			t.Fatalf("foreign backup not isolated: %+v", entry)
		}
		if err := m.Complete(name, ""); err == nil {
			t.Fatal("foreign backup accepted")
		}
	}
	result, err := m.Prune(Options{})
	if err != nil || !slices.Equal(result.Removed, []string{"our-latest", "our-backup"}) {
		t.Fatalf("incorrect isolation: %+v %v", result, err)
	}
	for index := range variants {
		exists(t, filepath.Join(m.root, fmt.Sprintf("foreign-%d", index)), true)
	}
}

func TestCompletionChecksContentsAndPreservesOldVersion(t *testing.T) {
	m, _ := fixture(t)
	makeBackup(t, m, "upgrade")
	if err := m.Complete("upgrade", "4.1.19"); err == nil {
		t.Fatal("new version was written as backup version")
	}
	exists(t, filepath.Join(m.root, "upgrade", MarkerName), false)
	if err := m.Complete("upgrade", ""); err != nil {
		t.Fatal(err)
	}
	before := entryNamed(t, m, "upgrade")
	if before.Version != "4.1.18" || !before.Managed {
		t.Fatalf("old version missing: %+v", before)
	}
	if err := m.Complete("upgrade", "4.1.18"); err != nil {
		t.Fatal(err)
	}
	after := entryNamed(t, m, "upgrade")
	if after.CompletedAt != before.CompletedAt {
		t.Fatal("idempotent completion changed retention order")
	}
	makeBackup(t, m, "empty-database")
	write(t, filepath.Join(m.root, "empty-database", "database"), "")
	if err := m.Complete("empty-database", ""); err == nil {
		t.Fatal("empty database was accepted")
	}
	makeBackup(t, m, "wrong-config")
	write(t, filepath.Join(m.root, "wrong-config", "service.json"), `{"serviceName":"other","dataDirectory":"/other"}`)
	if err := m.Complete("wrong-config", ""); err == nil {
		t.Fatal("wrong service configuration accepted")
	}
}

func TestMissingRootDoesNotCreateDirectories(t *testing.T) {
	m, _ := fixture(t)
	if err := os.Remove(m.root); err != nil {
		t.Fatal(err)
	}
	listing, err := m.List()
	if err != nil || len(listing.Entries) != 0 {
		t.Fatalf("missing directory listing: %+v %v", listing, err)
	}
	result, err := m.Prune(Options{})
	if err != nil || len(result.Removed) != 0 {
		t.Fatalf("missing directory cleanup: %+v %v", result, err)
	}
	exists(t, m.root, false)
	if err := m.Complete("backup", ""); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("complete should require existing backup: %v", err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink creation unavailable: %v", err)
		}
		t.Fatal(err)
	}
}

func TestRejectsSymlinkRootsAncestorsEntriesAndContents(t *testing.T) {
	m, base := fixture(t)
	symlink(t, m.root, filepath.Join(base, "root-link"))
	if _, err := New(filepath.Join(base, "root-link"), m.identity); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("symlink root accepted: %v", err)
	}
	if _, err := New(filepath.Join(base, "root-link", "missing-child"), m.identity); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("symlink ancestor accepted: %v", err)
	}
	outside := filepath.Join(base, "outside")
	if err := os.Mkdir(outside, 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(outside, "sentinel"), "must remain")
	symlink(t, outside, filepath.Join(m.root, "outside-link"))
	completedBackup(t, m, "latest", time.Hour)
	completedBackup(t, m, "linked-contents", 60*24*time.Hour)
	symlink(t, outside, filepath.Join(m.root, "linked-contents", "escape"))
	makeBackup(t, m, "linked-marker")
	symlink(t, filepath.Join(outside, "sentinel"), filepath.Join(m.root, "linked-marker", MarkerName))
	if err := m.Complete("linked-marker", ""); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("linked marker accepted: %v", err)
	}
	if err := m.Complete("outside-link", ""); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("linked backup accepted: %v", err)
	}
	result, err := m.Prune(Options{})
	if err != nil || !slices.Equal(result.Removed, []string{"latest"}) {
		t.Fatalf("unsafe backup cleanup: %+v %v", result, err)
	}
	for _, name := range []string{"outside-link", "linked-contents", "linked-marker"} {
		if entry := entryNamed(t, m, name); !entry.Protected || entry.Eligible {
			t.Fatalf("linked backup not protected: %+v", entry)
		}
	}
	raw, err := os.ReadFile(filepath.Join(outside, "sentinel"))
	if err != nil || string(raw) != "must remain" {
		t.Fatalf("outside file changed: %s %v", raw, err)
	}
}

func TestLockSymlinkCannotModifyExternalFile(t *testing.T) {
	m, base := fixture(t)
	outside := filepath.Join(base, "outside-lock")
	write(t, outside, "untouched")
	symlink(t, outside, filepath.Join(m.root, LockName))
	if _, err := m.List(); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("lock symlink accepted: %v", err)
	}
	raw, err := os.ReadFile(outside)
	if err != nil || string(raw) != "untouched" {
		t.Fatalf("outside lock was modified: %s %v", raw, err)
	}
}

func TestRejectsTraversalAndLiveDataRoots(t *testing.T) {
	m, _ := fixture(t)
	completedBackup(t, m, "latest", 0)
	for _, name := range []string{"..", ".", "../latest", `..\latest`, "/latest", `C:\latest`, "nested/child", "backup:stream"} {
		if _, err := m.Prune(Options{Names: []string{name}}); !errors.Is(err, ErrUnsafe) {
			t.Fatalf("unsafe prune name %q: %v", name, err)
		}
		if err := m.Complete(name, ""); !errors.Is(err, ErrUnsafe) {
			t.Fatalf("unsafe completion name %q: %v", name, err)
		}
	}
	for _, root := range []string{m.identity.DataDir, filepath.Join(m.identity.DataDir, "backups"), filepath.Dir(m.identity.DataDir), filepath.Dir(m.identity.Config)} {
		if _, err := New(root, m.identity); err == nil {
			t.Fatalf("live data root accepted: %s", root)
		}
	}
	exists(t, filepath.Join(m.root, "latest"), true)
}

func TestSharedFileLockAndConcurrentCleanup(t *testing.T) {
	m, _ := fixture(t)
	completedBackup(t, m, "latest", 0)
	held, err := bolt.Open(filepath.Join(m.root, LockName), 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.lockTimeout = 20 * time.Millisecond
	if _, err := m.List(); !errors.Is(err, ErrBusy) {
		t.Fatalf("list ignored lock: %v", err)
	}
	if _, err := m.Prune(Options{}); !errors.Is(err, ErrBusy) {
		t.Fatalf("prune ignored lock: %v", err)
	}
	if err := m.Complete("latest", ""); !errors.Is(err, ErrBusy) {
		t.Fatalf("complete ignored lock: %v", err)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 8; index++ {
		completedBackup(t, m, fmt.Sprintf("old-%d", index), time.Duration(index+1)*time.Hour)
	}
	m.lockTimeout = 5 * time.Second
	var group sync.WaitGroup
	errorsCh := make(chan error, 8)
	removed := make(chan []string, 8)
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			result, err := m.Prune(Options{})
			errorsCh <- err
			removed <- result.Removed
		}()
	}
	group.Wait()
	close(errorsCh)
	close(removed)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for names := range removed {
		for _, name := range names {
			if seen[name] {
				t.Fatalf("backup removed twice: %s", name)
			}
			seen[name] = true
		}
	}
	if len(seen) != 9 {
		t.Fatalf("wanted 9 removed backups, got %v", seen)
	}
	listing, err := m.List()
	if err != nil || len(listing.Entries) != 0 {
		t.Fatalf("concurrent cleanup retained wrong backups: %+v %v", listing, err)
	}
}

func TestRemovalFailureReturnsPartialResultWithoutOverstatingBytes(t *testing.T) {
	m, _ := fixture(t)
	completedBackup(t, m, "latest", 0)
	completedBackup(t, m, "keep", time.Hour)
	completedBackup(t, m, "cannot-remove", 2*time.Hour)
	completedBackup(t, m, "can-remove", 3*time.Hour)
	listing, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	retained := entryNamed(t, m, "cannot-remove")
	m.removeAll = func(root *os.Root, name string) error {
		if name == "cannot-remove" {
			return os.ErrPermission
		}
		return root.RemoveAll(name)
	}
	result, err := m.Prune(Options{})
	if !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), "cannot-remove") {
		t.Fatalf("failed deletion not reported: %v", err)
	}
	if !slices.Equal(result.Removed, []string{"latest", "keep", "can-remove"}) || result.FreedBytes != listing.TotalBytes-retained.SizeBytes {
		t.Fatalf("incorrect partial result: %+v", result)
	}
	exists(t, filepath.Join(m.root, "cannot-remove"), true)
	exists(t, filepath.Join(m.root, "can-remove"), false)
}
