package transferstore

import (
	"context"
	"errors"
	"fmt"
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
	syncPayloadOrdered = func(file *os.File) error {
		log.add("payload " + filepath.Base(file.Name()))
		return payload(file)
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
			if log.count("dir ") != 0 || log.count("forget") != 0 || registry.pending() != len(keys) {
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
			if tail[4] != fmt.Sprintf("forget-batch %d", len(keys)) || log.count("dir ") != 4 || registry.pending() != 0 {
				t.Fatalf("records were not cleared in one batch after the directory syncs: %v", tail)
			}
			// A second barrier has nothing left to do.
			if err := local.Sync(context.Background()); err != nil || len(log.snapshot()) != len(events) {
				t.Fatalf("empty barrier did work: %v", err)
			}
			if err := local.Close(); err != nil {
				t.Fatal(err)
			}
			if log.count("release") != 2 {
				t.Fatalf("namespaces were not released once each: %v", log.snapshot())
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
	want := []string{"forget-batch 3", "release"}
	if got := events[len(events)-2:]; got[0] != want[0] || got[1] != want[1] || log.count("dir ") != 2 || registry.pending() != 0 {
		t.Fatalf("Close did not finish deferred durability before releasing: %v", events)
	}
}

func TestLocalFailedBarrierKeepsRecordsAndNamespaces(t *testing.T) {
	if !deferredDurabilitySupported {
		t.Skip("durability is never deferred on this platform")
	}
	root := t.TempDir()
	log := &eventLog{}
	hookSyncs(t, log, true)
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
	if registry.pending() != 2 || log.count("forget") != 0 {
		t.Fatalf("records were cleared without a durable barrier: %v", log.snapshot())
	}
	_ = local.Close()
	if log.count("release") != 0 || registry.pending() != 2 {
		t.Fatalf("namespace released after a failed barrier: %v", log.snapshot())
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
	defer local.Close()
	local.SetTempRegistry(registry)
	for _, key := range []string{"a", "b"} {
		if result, err := local.Write(context.Background(), key, strings.NewReader("value"), WriteOptions{Size: 5, Overwrite: true}); err != nil || !result.Committed {
			t.Fatalf("write: %+v %v", result, err)
		}
	}
	// Without DeferDurability, Write returns only after the record is cleared
	// and nothing is left for a barrier.
	if log.count("payload ") != 0 || log.count("forget") != 2 || registry.pending() != 0 {
		t.Fatalf("immediate writes changed behavior: %v", log.snapshot())
	}
	before := len(log.snapshot())
	if err := local.Sync(context.Background()); err != nil || len(log.snapshot()) != before {
		t.Fatalf("barrier found deferred work after immediate writes: %v", err)
	}
}
