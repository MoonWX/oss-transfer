package storageconfig

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/state"
)

func testConfig(alias string) Config {
	pathStyle, v2 := true, false
	return Config{
		Alias: alias, Endpoint: "https://storage.example:8443", AccessKey: " access with spaces ",
		SecretKey: " secret with spaces ", SessionToken: " session with spaces ", Region: "test-region",
		UseV2SignerType: &v2, PathStyleEnable: &pathStyle,
	}
}

func openTestStore(t *testing.T, path string) *state.Store {
	t.Helper()
	store, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func assertSnapshot(t *testing.T, source SnapshotSource, expected Snapshot) {
	t.Helper()
	actual, err := source.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatal("persisted snapshot contents or revision changed unexpectedly")
	}
}

func TestMasterConcurrentUpdatesAndDurableCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.db")
	db := openTestStore(t, path)
	master := NewMaster(db)
	for _, alias := range []string{"first", "second"} {
		if _, err := master.Create(testConfig(alias)); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	revisions := make(chan uint64, 2)
	failures := make(chan error, 2)
	for _, alias := range []string{"first", "second"} {
		wg.Add(1)
		go func(alias string) {
			defer wg.Done()
			<-start
			config := testConfig(alias)
			config.Endpoint = "https://" + alias + ".example"
			// Separate wrappers must still share the database transaction boundary.
			updated, err := NewMaster(db).Update(alias, config)
			if err != nil {
				failures <- err
				return
			}
			revisions <- updated.Revision
		}(alias)
	}
	close(start)
	wg.Wait()
	close(failures)
	close(revisions)
	for err := range failures {
		t.Fatal(err)
	}
	seen := map[uint64]bool{}
	for revision := range revisions {
		seen[revision] = true
	}
	if len(seen) != 2 || !seen[3] || !seen[4] {
		t.Fatal("concurrent updates did not allocate distinct monotonic revisions")
	}
	expected := Snapshot{Revision: 4, Connections: []Config{testConfig("first"), testConfig("second")}}
	for index := range expected.Connections {
		expected.Connections[index].Endpoint = "https://" + expected.Connections[index].Alias + ".example"
	}
	assertSnapshot(t, master, expected)
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("credential database permissions must be 0600", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openTestStore(t, path)
	master = NewMaster(db)
	assertSnapshot(t, master, expected)
	copy, err := master.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	copy.Connections[0].SecretKey = "modified by caller"
	*copy.Connections[0].PathStyleEnable = false
	assertSnapshot(t, master, expected)
	if _, err := master.Create(expected.Connections[0]); !errors.Is(err, ErrExists) {
		t.Fatal("duplicate create replaced a connection", err)
	}
	if _, err := master.Update("missing", testConfig("missing")); !errors.Is(err, ErrNotFound) {
		t.Fatal("update created a missing connection", err)
	}
	if _, err := master.Delete("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleting a missing alias consumed a revision", err)
	}
	assertSnapshot(t, master, expected)
}

func TestAgentSnapshotDeletionFenceAndConflictsSurviveRestart(t *testing.T) {
	master := NewMaster(openTestStore(t, filepath.Join(t.TempDir(), "master.db")))
	if _, err := master.Create(testConfig("first")); err != nil {
		t.Fatal(err)
	}
	old, err := master.Create(testConfig("second"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "agent.db")
	db := openTestStore(t, path)
	agent := NewAgent(db)
	if changed, err := agent.ApplySnapshot(old); err != nil || !changed {
		t.Fatal("initial configuration was not applied", err)
	}
	reordered := Snapshot{Revision: old.Revision, Connections: []Config{old.Connections[1], old.Connections[0]}}
	if changed, err := agent.ApplySnapshot(reordered); err != nil || changed {
		t.Fatal("same contents in another order were not idempotent", err)
	}
	conflict := Snapshot{Revision: old.Revision, Connections: []Config{testConfig("first"), testConfig("second")}}
	conflict.Connections[0].SecretKey = "different secret at same revision"
	if changed, err := agent.ApplySnapshot(conflict); !errors.Is(err, ErrRevisionConflict) || changed {
		t.Fatal("same revision with different content accepted", err)
	}
	assertSnapshot(t, agent, old)
	deleted, err := master.Delete("first")
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := agent.ApplySnapshot(deleted); err != nil || !changed {
		t.Fatal("deletion snapshot failed", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	agent = NewAgent(openTestStore(t, path))
	if changed, err := agent.ApplySnapshot(old); !errors.Is(err, ErrStaleRevision) || changed {
		t.Fatal("old snapshot restored deleted credentials after restart", err)
	}
	assertSnapshot(t, agent, deleted)
	invalidBatch := Snapshot{Revision: deleted.Revision + 1, Connections: []Config{testConfig("valid-first"), testConfig("invalid-last")}}
	invalidBatch.Connections[1].Endpoint = "https://storage.example/private/path"
	if changed, err := agent.ApplySnapshot(invalidBatch); !errors.Is(err, ErrInvalid) || changed {
		t.Fatal("invalid snapshot partially applied", err)
	}
	assertSnapshot(t, agent, deleted)
}

func TestConcurrentSnapshotsKeepNewestAuthoritativeRevision(t *testing.T) {
	agent := NewAgent(openTestStore(t, filepath.Join(t.TempDir(), "agent.db")))
	old := Snapshot{Revision: 8, Connections: []Config{testConfig("deleted")}}
	newer := Snapshot{Revision: 9, Connections: []Config{}}
	var wg sync.WaitGroup
	start := make(chan struct{})
	failures := make(chan error, 2)
	for _, incoming := range []Snapshot{old, newer} {
		wg.Add(1)
		go func(incoming Snapshot) {
			defer wg.Done()
			<-start
			_, err := agent.ApplySnapshot(incoming)
			failures <- err
		}(incoming)
	}
	close(start)
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil && !errors.Is(err, ErrStaleRevision) {
			t.Fatal(err)
		}
	}
	assertSnapshot(t, agent, newer)
}

func TestInvalidConfigurationLeavesSnapshotUnchanged(t *testing.T) {
	master := NewMaster(openTestStore(t, filepath.Join(t.TempDir(), "master.db")))
	expected, err := master.Create(testConfig("original"))
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Config){
		"empty alias":      func(c *Config) { c.Alias = "" },
		"alias separator":  func(c *Config) { c.Alias = "prefix/alias" },
		"alias traversal":  func(c *Config) { c.Alias = ".." },
		"empty endpoint":   func(c *Config) { c.Endpoint = "" },
		"endpoint scheme":  func(c *Config) { c.Endpoint = "file:///tmp/data" },
		"endpoint user":    func(c *Config) { c.Endpoint = "https://user:secret@storage.example" },
		"endpoint path":    func(c *Config) { c.Endpoint += "/api" },
		"encoded path":     func(c *Config) { c.Endpoint += "/%2f" },
		"endpoint query":   func(c *Config) { c.Endpoint += "?token=private" },
		"empty query":      func(c *Config) { c.Endpoint += "?" },
		"empty fragment":   func(c *Config) { c.Endpoint += "#" },
		"endpoint port":    func(c *Config) { c.Endpoint = "https://storage.example:99999" },
		"empty access key": func(c *Config) { c.AccessKey = "" },
		"empty secret":     func(c *Config) { c.SecretKey = "" },
		"control secret":   func(c *Config) { c.SecretKey = "private-secret\n" },
		"invalid utf8":     func(c *Config) { c.SecretKey = string([]byte{0xff}) },
		"region spaces":    func(c *Config) { c.Region = "region with spaces" },
	} {
		t.Run(name, func(t *testing.T) {
			config := testConfig("original")
			change(&config)
			_, err := master.Update("original", config)
			if !errors.Is(err, ErrInvalid) {
				t.Fatal("invalid configuration was accepted")
			}
			if strings.Contains(err.Error(), "private-secret") || (config.Endpoint != "" && strings.Contains(err.Error(), config.Endpoint)) {
				t.Fatal("validation error contains configuration values")
			}
			assertSnapshot(t, master, expected)
		})
	}
	duplicate := Snapshot{Revision: 2, Connections: []Config{testConfig("same"), testConfig("same")}}
	agent := NewAgent(openTestStore(t, filepath.Join(t.TempDir(), "agent.db")))
	if _, err := agent.ApplySnapshot(duplicate); !errors.Is(err, ErrInvalid) {
		t.Fatal("duplicate aliases accepted", err)
	}
	assertSnapshot(t, agent, Snapshot{Connections: []Config{}})
}
