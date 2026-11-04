package transferstore

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// eventRegistry is a minimal tracked-temp registry that logs every durable
// step, so tests can check the order of syncs and record changes.
type eventRegistry struct {
	log        *eventLog
	records    map[string]LocalTempRecord
	namespaces map[string]LocalTempNamespace
	refs       map[string]int
	mu         sync.Mutex
}

type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(event string) {
	l.mu.Lock()
	l.events = append(l.events, event)
	l.mu.Unlock()
}

func (l *eventLog) count(prefix string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, event := range l.events {
		if strings.HasPrefix(event, prefix) {
			n++
		}
	}
	return n
}

func (l *eventLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

func newEventRegistry(log *eventLog) *eventRegistry {
	return &eventRegistry{log: log, records: map[string]LocalTempRecord{}, namespaces: map[string]LocalTempNamespace{}, refs: map[string]int{}}
}

func (r *eventRegistry) OwnerID() string { return strings.Repeat("c", 64) }
func (r *eventRegistry) Register(_ context.Context, record LocalTempRecord) error {
	r.mu.Lock()
	r.records[record.ID] = record
	r.mu.Unlock()
	r.log.add("register")
	return nil
}
func (r *eventRegistry) RegisterBatch(_ context.Context, records []LocalTempRecord) error {
	r.mu.Lock()
	for _, record := range records {
		r.records[record.ID] = record
	}
	r.mu.Unlock()
	r.log.add(fmt.Sprintf("register-batch %d", len(records)))
	return nil
}
func (r *eventRegistry) Forget(_ context.Context, id string) error {
	r.mu.Lock()
	delete(r.records, id)
	r.mu.Unlock()
	r.log.add("forget")
	return nil
}
func (r *eventRegistry) ForgetBatch(_ context.Context, ids []string) error {
	r.mu.Lock()
	for _, id := range ids {
		delete(r.records, id)
	}
	r.mu.Unlock()
	r.log.add(fmt.Sprintf("forget-batch %d", len(ids)))
	return nil
}
func (r *eventRegistry) PrepareNamespace(ctx context.Context, parent *os.Root, parentPath string) (LocalTempNamespace, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	namespace, ok := r.namespaces[parentPath]
	if !ok {
		var err error
		if namespace, err = CreateLocalTempNamespace(ctx, parent, parentPath, r.OwnerID()); err != nil {
			return namespace, err
		}
		r.namespaces[parentPath] = namespace
	}
	r.refs[parentPath]++
	return namespace, nil
}
func (r *eventRegistry) ReleaseNamespace(_ context.Context, namespace LocalTempNamespace) error {
	r.mu.Lock()
	r.refs[namespace.ParentPath]--
	r.mu.Unlock()
	r.log.add("release")
	return nil
}
func (r *eventRegistry) pending() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.records)
}

// hookSyncs counts ordered payload syncs and directory fsyncs. The payload
// hook also checks that the destination is not yet visible.
func hookSyncs(t *testing.T, log *eventLog, failDirectory bool) {
	t.Helper()
	payload, directory := syncPayloadOrdered, syncDirectory
	t.Cleanup(func() { syncPayloadOrdered, syncDirectory = payload, directory })
	syncPayloadOrdered = func(file *os.File, cache *orderingCache) error {
		log.add("payload " + filepath.Base(file.Name()))
		return payload(file, cache)
	}
	syncDirectory = func(file *os.File) error {
		log.add("dir " + file.Name())
		if failDirectory {
			return errors.New("injected directory sync failure")
		}
		return directory(file)
	}
}

func writeDeferred(t *testing.T, local *Local, keys []string, overwrite bool) {
	t.Helper()
	var group sync.WaitGroup
	for _, key := range keys {
		group.Add(1)
		go func() {
			defer group.Done()
			result, err := local.Write(context.Background(), key, strings.NewReader("payload of "+key), WriteOptions{Size: int64(len("payload of " + key)), Overwrite: overwrite, DeferDurability: true})
			if err != nil || !result.Committed {
				t.Errorf("write %s: %+v %v", key, result, err)
			}
		}()
	}
	group.Wait()
}

// countBatches sums the sizes logged as "<prefix> N".
func countBatches(log *eventLog, prefix string) (batches, total int) {
	for _, event := range log.snapshot() {
		var n int
		if _, err := fmt.Sscanf(event, prefix+" %d", &n); err == nil {
			batches++
			total += n
		}
	}
	return batches, total
}

func tempNames(t *testing.T, root string) []string {
	t.Helper()
	var names []string
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && strings.HasSuffix(entry.Name(), ".tmp") {
			names = append(names, path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return names
}

func TestLocalDeferredWritesBatchDirectorySyncsBeforeForget(t *testing.T) {
	if !deferredDurabilitySupported {
		t.Skip("durability is never deferred on this platform")
	}
	for _, overwrite := range []bool{true, false} {
		t.Run(fmt.Sprintf("overwrite=%v", overwrite), func(t *testing.T) {
			root := t.TempDir()
			log := &eventLog{}
			hookSyncs(t, log, false)
			registry := newEventRegistry(log)
			local, err := NewLocal(root)
			if err != nil {
				t.Fatal(err)
			}
			local.SetTempRegistry(registry)
			var keys []string
			for i := range 24 {
				keys = append(keys, fmt.Sprintf("d%d/f%02d", i%2, i))
			}
			writeDeferred(t, local, keys, overwrite)
			for _, key := range keys {
				if body, err := os.ReadFile(filepath.Join(root, key)); err != nil || string(body) != "payload of "+key {
					t.Fatalf("published %s is incomplete: %q %v", key, body, err)
				}
			}
			// Every payload is ordered before publication, but no directory
			// is synced and no record is cleared until the barrier.
			if n := log.count("payload "); n != len(keys) {
				t.Fatalf("ordered payload syncs: got %d want %d", n, len(keys))
			}
			// Empty temporary files are registered ahead in doubling batches
			// (1, 2, 4, 8 per directory), not one commit per object.
			batches, reserved := countBatches(log, "register-batch")
			if log.count("register ") != 0 || batches != 8 || reserved != 30 {
				t.Fatalf("registrations were not batched: %d batches, %d files", batches, reserved)
			}
			if log.count("dir ") != 0 || log.count("forget") != 0 || registry.pending() != reserved {
				t.Fatalf("durability work was not deferred: %v", log.snapshot())
			}
			if err := local.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			// Two destination and two staging directories, each synced once,
			// then one batched record deletion.
			events := log.snapshot()
			tail := events[len(events)-5:]
			for _, event := range tail[:4] {
				if !strings.HasPrefix(event, "dir ") {
					t.Fatalf("barrier did not sync each touched directory once before forgetting: %v", tail)
				}
			}
			if tail[4] != fmt.Sprintf("forget-batch %d", len(keys)) || log.count("dir ") != 4 || registry.pending() != reserved-len(keys) {
				t.Fatalf("records were not cleared in one batch after the directory syncs: %v", tail)
			}
			// A second barrier has nothing left to do.
			if err := local.Sync(context.Background()); err != nil || len(log.snapshot()) != len(events) {
				t.Fatalf("empty barrier did work: %v", err)
			}
			if err := local.Close(); err != nil {
				t.Fatal(err)
			}
			// Close removes the unused, empty reservations and clears their
			// records in one batch without another directory fsync.
			after := log.snapshot()[len(events):]
			if registry.pending() != 0 || log.count("release") != 2 || log.count("dir ") != 4 || log.count(fmt.Sprintf("forget-batch %d", reserved-len(keys))) != 1 {
				t.Fatalf("Close did not clean up reservations once: %v", after)
			}
			if names := tempNames(t, root); len(names) != 0 {
				t.Fatalf("temporary files remain: %v", names)
			}
		})
	}
}

func TestLocalCloseSyncsDeferredWrites(t *testing.T) {
	if !deferredDurabilitySupported {
		t.Skip("durability is never deferred on this platform")
	}
	root := t.TempDir()
	log := &eventLog{}
	hookSyncs(t, log, false)
	registry := newEventRegistry(log)
	local, err := NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	local.SetTempRegistry(registry)
	writeDeferred(t, local, []string{"a", "b", "c"}, true)
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	events := log.snapshot()
	_, reserved := countBatches(log, "register-batch")
	if got := events[len(events)-2:]; got[0] != fmt.Sprintf("forget-batch %d", reserved) || got[1] != "release" || log.count("dir ") != 2 || registry.pending() != 0 {
		t.Fatalf("Close did not finish deferred durability before releasing: %v", events)
	}
}

func TestLocalFailedBarrierPoisonsLaterBarriers(t *testing.T) {
	if !deferredDurabilitySupported {
		t.Skip("durability is never deferred on this platform")
	}
	root := t.TempDir()
	log := &eventLog{}
	fail := true
	directory := syncDirectory
	t.Cleanup(func() { syncDirectory = directory })
	syncDirectory = func(file *os.File) error {
		log.add("dir " + file.Name())
		if fail {
			return errors.New("injected directory sync failure")
		}
		return directory(file)
	}
	registry := newEventRegistry(log)
	local, err := NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	local.SetTempRegistry(registry)
	writeDeferred(t, local, []string{"x/a", "x/b"}, false)
	if err := local.Sync(context.Background()); err == nil {
		t.Fatal("failed directory sync was not reported")
	}
	// Restart recovery still owns both temporary names, through the namespace.
	if registry.pending() < 2 || log.count("forget") != 0 {
		t.Fatalf("records were cleared without a durable barrier: %v", log.snapshot())
	}
	// Even once the disk recovers, nothing this Local wrote may be reported
	// durable: the failed directories were never synced successfully.
	fail = false
	writeDeferred(t, local, []string{"x/c"}, false)
	if err := local.Sync(context.Background()); err == nil {
		t.Fatal("barrier succeeded after an earlier failure")
	}
	// The registry decides whether the namespace can go, so Close releases it.
	_ = local.Close()
	if log.count("release") != 1 || log.count("forget") != 0 {
		t.Fatalf("namespace was not released, or records were cleared: %v", log.snapshot())
	}
}

func TestLocalImmediateWriteKeepsPerFileDurability(t *testing.T) {
	root := t.TempDir()
	log := &eventLog{}
	hookSyncs(t, log, false)
	registry := newEventRegistry(log)
	local, err := NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	local.SetTempRegistry(registry)
	for _, key := range []string{"a", "b"} {
		if result, err := local.Write(context.Background(), key, strings.NewReader("value"), WriteOptions{Size: 5, Overwrite: true}); err != nil || !result.Committed {
			t.Fatalf("write: %+v %v", result, err)
		}
	}
	// Without DeferDurability, Write returns only after its record is cleared
	// and nothing is left for a barrier; only an unused reservation remains.
	_, reserved := countBatches(log, "register-batch")
	if log.count("payload ") != 0 || log.count("forget") != 2 || registry.pending() != reserved-2 {
		t.Fatalf("immediate writes changed behavior: %v", log.snapshot())
	}
	before := len(log.snapshot())
	if err := local.Sync(context.Background()); err != nil || len(log.snapshot()) != before {
		t.Fatalf("barrier found deferred work after immediate writes: %v", err)
	}
	if err := local.Close(); err != nil || registry.pending() != 0 {
		t.Fatalf("Close left reservations: %v", err)
	}
}

func TestLocalSkipIsCoveredByNextBarrier(t *testing.T) {
	if !deferredDurabilitySupported {
		t.Skip("durability is never deferred on this platform")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "s"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"s/stat", "s/engine"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("existing"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	log := &eventLog{}
	hookSyncs(t, log, false)
	local, err := NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	local.SetTempRegistry(newEventRegistry(log))
	result, err := local.Write(context.Background(), "s/stat", strings.NewReader("new"), WriteOptions{Size: 3, DeferDurability: true})
	if err != nil || !result.Skipped {
		t.Fatalf("expected skip: %+v %v", result, err)
	}
	if err := local.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if log.count("dir ") != 1 {
		t.Fatalf("a skipped write did not cover its directory entry: %v", log.snapshot())
	}
	if err := local.DeferExisting(context.Background(), "s/engine"); err != nil {
		t.Fatal(err)
	}
	if err := local.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if log.count("dir ") != 2 {
		t.Fatalf("DeferExisting did not cover the directory entry: %v", log.snapshot())
	}
}

// A destination directory replaced while a Local caches it is resolved again,
// so no write goes through the stale handle.
func TestLocalDirectoryCacheRevalidates(t *testing.T) {
	root := t.TempDir()
	local, err := NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	write := func(key, body string) {
		t.Helper()
		if result, err := local.Write(context.Background(), key, strings.NewReader(body), WriteOptions{Size: int64(len(body)), Overwrite: true, DeferDurability: true}); err != nil || !result.Committed {
			t.Fatalf("write %s: %+v %v", key, result, err)
		}
	}
	write("d/a", "first")
	if err := os.Rename(filepath.Join(root, "d"), filepath.Join(root, "moved")); err != nil {
		t.Fatal(err)
	}
	write("d/b", "second")
	if _, err := os.Stat(filepath.Join(root, "moved", "b")); !os.IsNotExist(err) {
		t.Fatalf("write followed the moved directory: %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(root, "d", "b")); err != nil || string(body) != "second" {
		t.Fatalf("write did not land in the current directory: %q %v", body, err)
	}
	if err := local.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
}
