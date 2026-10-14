package agentservice

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

type crashTempReader struct {
	owner string
	first bool
}

func (r *crashTempReader) Read(buffer []byte) (int, error) {
	if !r.first {
		r.first = true
		n := min(len(buffer), 64*1024)
		for i := range buffer[:n] {
			buffer[i] = 0x71
		}
		return n, nil
	}
	// io.Copy has written the first chunk before asking for the next one.
	fmt.Println("READY " + r.owner)
	for {
		time.Sleep(time.Hour)
	}
}

func TestTempRegistryCrashHelper(t *testing.T) {
	if os.Getenv("OSS_TRANSFER_TEMP_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	db, err := state.Open(os.Getenv("OSS_TRANSFER_TEMP_DB"))
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
	local, err := transferstore.NewLocal(os.Getenv("OSS_TRANSFER_TEMP_ROOT"))
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("OSS_TRANSFER_TEMP_POINT") == "before-register" {
		local.SetTempRegistry(&pauseBeforeRegistration{LocalTempRegistry: registry})
	} else {
		local.SetTempRegistry(registry)
	}
	_, err = local.Write(context.Background(), "destination.bin", &crashTempReader{owner: registry.OwnerID()}, transferstore.WriteOptions{Size: 64 << 20, Overwrite: true})
	t.Fatalf("crash helper unexpectedly returned: %v", err)
}

type tempHelper struct {
	command *exec.Cmd
	owner   string
	stopped bool
}

func startTempHelper(t *testing.T, database, root string) *tempHelper {
	return startTempHelperAt(t, database, root, "payload")
}
func startTempHelperAt(t *testing.T, database, root, point string) *tempHelper {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestTempRegistryCrashHelper$", "-test.v")
	command.Env = append(os.Environ(), "OSS_TRANSFER_TEMP_HELPER=1", "OSS_TRANSFER_TEMP_DB="+database, "OSS_TRANSFER_TEMP_ROOT="+root, "OSS_TRANSFER_TEMP_POINT="+point)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = os.Stderr
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	helper := &tempHelper{command: command}
	t.Cleanup(func() { helper.kill() })
	ready := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if strings.HasPrefix(scanner.Text(), "READY ") {
				ready <- strings.TrimPrefix(scanner.Text(), "READY ")
				return
			}
		}
		ready <- ""
	}()
	select {
	case helper.owner = <-ready:
		if helper.owner == "" {
			t.Fatal("crash helper exited before writing payload")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("crash helper did not write payload")
	}
	return helper
}
func (h *tempHelper) kill() {
	if !h.stopped {
		h.stopped = true
		_ = h.command.Process.Kill()
		_ = h.command.Wait()
	}
}

func openTempRegistry(t *testing.T, path string) (*LocalTempRegistry, *state.Store) {
	t.Helper()
	db, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewLocalTempRegistry(db)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { registry.Close(); db.Close() })
	return registry, db
}

func tempRecords(t *testing.T, db *state.Store) []transferstore.LocalTempRecord {
	t.Helper()
	var records []transferstore.LocalTempRecord
	if err := db.View(func(tx *state.Tx) error {
		return tx.Scan(localTemporaryBucket, "", "", 100, func(key string, _ json.RawMessage) error {
			var record transferstore.LocalTempRecord
			_, err := tx.Get(localTemporaryBucket, key, &record)
			records = append(records, record)
			return err
		})
	}); err != nil {
		t.Fatal(err)
	}
	return records
}

func TestLocalTempRecoveryAfterKillPreservesOtherAgentAndUserFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "destination.bin"), []byte("previous complete target"), 0600); err != nil {
		t.Fatal(err)
	}
	userName := ".oss-transfer-0123456789abcdef0123456789abcdef.tmp"
	if err := os.WriteFile(filepath.Join(root, userName), []byte("user file"), 0600); err != nil {
		t.Fatal(err)
	}
	firstDB, secondDB := filepath.Join(t.TempDir(), "agent.db"), filepath.Join(t.TempDir(), "agent.db")
	first := startTempHelper(t, firstDB, root)
	second := startTempHelper(t, secondDB, root)
	if first.owner == second.owner {
		t.Fatal("independent databases share a temp owner")
	}
	// bbolt must prevent recovery opening an actively running owner's database.
	if duplicate, err := state.Open(firstDB); err == nil {
		duplicate.Close()
		t.Fatal("active database was not exclusive")
	}
	first.kill()
	registry, db := openTempRegistry(t, firstDB)
	if registry.OwnerID() != first.owner {
		t.Fatal("restart changed database owner")
	}
	records := tempRecords(t, db)
	if len(records) != 1 {
		t.Fatalf("payload was written without durable temp record: %+v", records)
	}
	if info, err := os.Stat(filepath.Join(records[0].ParentPath, records[0].Name)); err != nil || info.Size() <= 0 || info.Size() > 64*1024 {
		t.Fatalf("expected incomplete payload missing: %v %v", info, err)
	}
	report, err := registry.Recover(context.Background())
	if err != nil || report.Removed != 1 || len(report.Retained) != 0 {
		t.Fatalf("recovery failed: %+v %v", report, err)
	}
	if len(tempRecords(t, db)) != 0 {
		t.Fatal("successful cleanup left journal record")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	otherFound := false
	for _, entry := range entries {
		if strings.Contains(entry.Name(), second.owner) {
			otherFound = true
		}
	}
	if !otherFound {
		t.Fatal("cleanup deleted another live Agent's temporary file")
	}
	for name, expected := range map[string]string{"destination.bin": "previous complete target", userName: "user file"} {
		if body, err := os.ReadFile(filepath.Join(root, name)); err != nil || string(body) != expected {
			t.Fatalf("cleanup damaged %s: %v", name, err)
		}
	}
	second.kill()
	secondRegistry, _ := openTempRegistry(t, secondDB)
	if report, err := secondRegistry.Recover(context.Background()); err != nil || report.Removed != 1 {
		t.Fatalf("other owner recovery failed: %+v %v", report, err)
	}
}

func TestLocalTempRecoveryRetainsReplacedFile(t *testing.T) {
	root, path := t.TempDir(), filepath.Join(t.TempDir(), "agent.db")
	helper := startTempHelper(t, path, root)
	helper.kill()
	registry, db := openTempRegistry(t, path)
	records := tempRecords(t, db)
	if len(records) != 1 {
		t.Fatal("missing record")
	}
	file := filepath.Join(records[0].ParentPath, records[0].Name)
	if err := os.Rename(file, filepath.Join(root, "held-original")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("replacement user file"), 0600); err != nil {
		t.Fatal(err)
	}
	report, err := registry.Recover(context.Background())
	if err != nil || report.Removed != 0 || len(report.Retained) != 1 {
		t.Fatalf("replacement was not retained: %+v %v", report, err)
	}
	if body, err := os.ReadFile(file); err != nil || string(body) != "replacement user file" {
		t.Fatal("replacement was removed")
	}
	if len(tempRecords(t, db)) != 1 {
		t.Fatal("unsafe cleanup forgot unresolved record")
	}
}

func TestLocalTempRecoveryRetainsReplacedDirectory(t *testing.T) {
	base, path := t.TempDir(), filepath.Join(t.TempDir(), "agent.db")
	root := filepath.Join(base, "mount")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	helper := startTempHelper(t, path, root)
	helper.kill()
	registry, db := openTempRegistry(t, path)
	records := tempRecords(t, db)
	if len(records) != 1 {
		t.Fatal("missing record")
	}
	if err := os.Rename(root, filepath.Join(base, "unmounted")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(records[0].ParentPath, records[0].Name)
	if err := os.MkdirAll(filepath.Dir(replacement), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replacement, []byte("different mount user file"), 0600); err != nil {
		t.Fatal(err)
	}
	report, err := registry.Recover(context.Background())
	if err != nil || report.Removed != 0 || len(report.Retained) != 2 || len(tempRecords(t, db)) != 1 {
		t.Fatalf("changed mount was not retained: %+v %v", report, err)
	}
	if body, err := os.ReadFile(replacement); err != nil || string(body) != "different mount user file" {
		t.Fatal("recovery damaged a different directory")
	}
}

func TestLocalTempFailedWritesClearJournalAndKeepTarget(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%v", cancel), func(t *testing.T) {
			registry, db := openTempRegistry(t, filepath.Join(t.TempDir(), "agent.db"))
			if _, err := registry.Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "target"), []byte("complete original"), 0600); err != nil {
				t.Fatal(err)
			}
			local, err := transferstore.NewLocal(root)
			if err != nil {
				t.Fatal(err)
			}
			defer local.Close()
			local.SetTempRegistry(registry)
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			var reader io.Reader = strings.NewReader("short payload")
			if cancel {
				reader = &cancelTempReader{stop: stop}
			}
			if result, err := local.Write(ctx, "target", reader, transferstore.WriteOptions{Size: 1024, Overwrite: true}); err == nil || result.Committed {
				t.Fatalf("failed input was published: %+v %v", result, err)
			}
			if len(tempRecords(t, db)) != 0 {
				t.Fatal("failed write left journal record")
			}
			assertLocalInventoryAndNoTemp(t, local, root, "target")
			if body, err := os.ReadFile(filepath.Join(root, "target")); err != nil || string(body) != "complete original" {
				t.Fatal("failed write damaged complete target")
			}
		})
	}
}

type cancelTempReader struct{ stop context.CancelFunc }

func (r *cancelTempReader) Read(buffer []byte) (int, error) {
	r.stop()
	return copy(buffer, "cancelled payload"), nil
}

func TestLocalTempNormalWriteClearsJournalAndRequiresRecovery(t *testing.T) {
	registry, db := openTempRegistry(t, filepath.Join(t.TempDir(), "agent.db"))
	root := t.TempDir()
	local, err := transferstore.NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	local.SetTempRegistry(registry)
	if _, err = local.Write(context.Background(), "file", strings.NewReader("value"), transferstore.WriteOptions{Size: 5}); err == nil {
		t.Fatal("I/O started before recovery")
	}
	if _, err := registry.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if result, err := local.Write(context.Background(), "file", strings.NewReader("value"), transferstore.WriteOptions{Size: 5}); err != nil || !result.Committed {
		t.Fatalf("tracked write failed: %+v %v", result, err)
	}
	if records := tempRecords(t, db); len(records) != 0 {
		t.Fatalf("normal completion leaked journal: %+v", records)
	}
	assertLocalInventoryAndNoTemp(t, local, root, "file")
	if _, err := registry.Recover(context.Background()); err == nil {
		t.Fatal("live recovery allowed")
	}
	reader, _, err := local.Open(context.Background(), "file", transferstore.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	contents, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || string(contents) != "value" {
		t.Fatal("committed content changed")
	}
}

// This barrier is deliberately before the file identity is written to the DB.
// Killing here reproduces the window observed in native Windows CI.
type pauseBeforeRegistration struct{ *LocalTempRegistry }

func (r *pauseBeforeRegistration) Register(context.Context, transferstore.LocalTempRecord) error {
	fmt.Println("READY " + r.OwnerID())
	for {
		time.Sleep(time.Hour)
	}
}

func assertLocalInventoryAndNoTemp(t *testing.T, local *transferstore.Local, root string, want ...string) {
	t.Helper()
	var actual []string
	if err := local.Scan(context.Background(), transferstore.ListOptions{}, func(object transferstore.Object) error { actual = append(actual, object.Key); return nil }); err != nil {
		t.Fatal(err)
	}
	slices.Sort(actual)
	slices.Sort(want)
	if !slices.Equal(actual, want) {
		t.Fatalf("inventory includes staging metadata or loses payload: got %v want %v", actual, want)
	}
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".tmp") {
			t.Errorf("temporary payload remains: %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLocalTempRecoveryAfterKillBeforeFileRegistration(t *testing.T) {
	root := t.TempDir()
	database := filepath.Join(t.TempDir(), "agent.db")
	first := startTempHelperAt(t, database, root, "before-register")
	// A second Agent is actively writing under the exact same shared root.
	secondDB := filepath.Join(t.TempDir(), "second.db")
	second := startTempHelper(t, secondDB, root)
	first.kill()
	registry, db := openTempRegistry(t, database)
	if records := tempRecords(t, db); len(records) != 0 {
		t.Fatalf("barrier did not precede registration: %+v", records)
	}
	var ownFile string
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.Contains(entry.Name(), first.owner) && strings.HasSuffix(entry.Name(), ".tmp") {
			ownFile = path
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Size() != 0 {
				t.Fatalf("unregistered file contains payload: %d", info.Size())
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if ownFile == "" {
		t.Fatal("file-create barrier did not leave a temporary file")
	}
	// Similar names outside the owned directory must never be swept.
	userFile := filepath.Join(root, filepath.Base(ownFile))
	if err := os.WriteFile(userFile, []byte("user file"), 0600); err != nil {
		t.Fatal(err)
	}
	report, err := registry.Recover(context.Background())
	if err != nil || report.Removed != 1 || len(report.Retained) != 0 {
		t.Fatalf("pre-registration recovery: %+v %v", report, err)
	}
	if _, err := os.Stat(ownFile); !os.IsNotExist(err) {
		t.Fatalf("unregistered empty file remains: %v", err)
	}
	if body, err := os.ReadFile(userFile); err != nil || string(body) != "user file" {
		t.Fatalf("user file was touched: %v", err)
	}
	second.kill()
	other, otherDB := openTempRegistry(t, secondDB)
	if records := tempRecords(t, otherDB); len(records) != 1 {
		t.Fatal("other Agent's journal damaged")
	}
	if report, err := other.Recover(context.Background()); err != nil || report.Removed != 1 || len(report.Retained) != 0 {
		t.Fatalf("other Agent lost its payload: %+v %v", report, err)
	}
}

type replacingTempInput struct {
	reader  io.Reader
	replace func()
}

func (r *replacingTempInput) Read(buffer []byte) (int, error) {
	if r.replace != nil {
		replace := r.replace
		r.replace = nil
		replace()
	}
	return r.reader.Read(buffer)
}

func TestLocalTempPublicationFailureRetainsSameNameReplacement(t *testing.T) {
	registry, db := openTempRegistry(t, filepath.Join(t.TempDir(), "agent.db"))
	if _, err := registry.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	local, err := transferstore.NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	local.SetTempRegistry(registry)
	if err := os.WriteFile(filepath.Join(root, "target"), []byte("existing target"), 0600); err != nil {
		t.Fatal(err)
	}
	var replacement string
	input := &replacingTempInput{reader: strings.NewReader("payload"), replace: func() {
		records := tempRecords(t, db)
		if len(records) != 1 {
			t.Fatal("replacement must occur after durable registration")
		}
		replacement = filepath.Join(records[0].ParentPath, records[0].Name)
		if err := os.Rename(replacement, filepath.Join(root, "original-temporary-held")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(replacement, []byte("replacement user file"), 0600); err != nil {
			t.Fatal(err)
		}
	}}
	result, err := local.Write(context.Background(), "target", input, transferstore.WriteOptions{Size: 7, Overwrite: true})
	if err == nil || result.Committed {
		t.Fatalf("replacement was published: %+v %v", result, err)
	}
	if data, err := os.ReadFile(replacement); err != nil || string(data) != "replacement user file" {
		t.Fatalf("deferred cleanup removed replacement: %q %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "target")); err != nil || string(data) != "existing target" {
		t.Fatal("failed publication changed target")
	}
	if len(tempRecords(t, db)) != 1 {
		t.Fatal("cleanup forgot unresolved replacement record")
	}
}
