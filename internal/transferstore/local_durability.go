package transferstore

import (
	"context"
	"errors"
	"os"
	"sync"
)

// Test hooks. Production code always uses the real system calls.
var (
	syncPayloadOrdered = orderedFileSync
	syncDirectory      = func(directory *os.File) error { return directory.Sync() }
)

// LocalTempBatchForgetter clears many temporary-file records in one durable
// commit. Registries without it receive one Forget call per record.
type LocalTempBatchForgetter interface {
	ForgetBatch(context.Context, []string) error
}

// LocalTempNamespaceReleaser is implemented by namespace registries that
// remove a staging directory once nobody uses it. Every successful
// PrepareNamespace holds one reference; Local releases it on Close after the
// records of every temporary file it created there have been forgotten.
type LocalTempNamespaceReleaser interface {
	ReleaseNamespace(context.Context, LocalTempNamespace) error
}

// localDurability holds the work that deferred writes still owe.
//
// A deferred write orders its payload before publication (orderedFileSync),
// publishes, removes its temporary name and returns. Sync then fsyncs every
// directory those writes changed (the destination directory, and the staging
// directory the temporary name was removed from), and only after that clears
// the temporary-file records. Until a record is cleared, restart recovery
// still owns the temporary name, so a crash at any point leaves either a
// complete published object or a registered temporary file to remove.
type localDurability struct {
	syncMu      sync.Mutex // one Sync at a time, so a returning Sync covers all earlier writes
	namespaceMu sync.Mutex // serializes PrepareNamespace calls
	mu          sync.Mutex
	dirs        map[string]*os.File // directory identity -> handle to fsync
	forget      []string            // temporary-file record IDs to clear after the fsyncs
	failed      bool                // a Sync failed; keep namespaces for restart recovery
	namespaces  map[string]LocalTempNamespace
}

// deferDirectories records the directories a deferred write changed and the
// temporary-file record to clear once they are durable.
func (s *Local) deferDirectories(id string, directories ...*os.Root) error {
	d := &s.durability
	for _, directory := range directories {
		info, err := directory.Stat(".")
		if err != nil {
			return err
		}
		key, err := storageIdentity(directory, ".", info)
		if err != nil {
			return err
		}
		d.mu.Lock()
		_, held := d.dirs[key]
		d.mu.Unlock()
		if held {
			continue
		}
		handle, err := directory.Open(".")
		if err != nil {
			return err
		}
		opened, err := handle.Stat()
		if err == nil && !os.SameFile(info, opened) {
			err = ErrTempIdentity
		}
		if err != nil {
			handle.Close()
			return err
		}
		d.mu.Lock()
		if _, held := d.dirs[key]; held {
			handle.Close()
		} else {
			if d.dirs == nil {
				d.dirs = make(map[string]*os.File)
			}
			d.dirs[key] = handle
		}
		d.mu.Unlock()
	}
	if id != "" {
		d.mu.Lock()
		d.forget = append(d.forget, id)
		d.mu.Unlock()
	}
	return nil
}

// Sync makes every deferred write that returned before the call durable, then
// clears their temporary-file records in one registry commit.
func (s *Local) Sync(ctx context.Context) error {
	d := &s.durability
	d.syncMu.Lock()
	defer d.syncMu.Unlock()
	d.mu.Lock()
	dirs, ids := d.dirs, d.forget
	d.dirs, d.forget = nil, nil
	d.mu.Unlock()
	var err error
	for _, directory := range dirs {
		err = errors.Join(err, syncDirectory(directory))
		directory.Close()
	}
	if err != nil {
		// The records stay; restart recovery removes whatever temporary names
		// reappear. Their staging directories must survive until then.
		d.mu.Lock()
		d.failed = true
		d.mu.Unlock()
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	ctx = context.WithoutCancel(ctx)
	if batch, ok := s.tempRegistry.(LocalTempBatchForgetter); ok {
		err = batch.ForgetBatch(ctx, ids)
	} else {
		for _, id := range ids {
			err = errors.Join(err, s.tempRegistry.Forget(ctx, id))
		}
	}
	if err != nil {
		d.mu.Lock()
		d.failed = true
		d.mu.Unlock()
	}
	return err
}

// tempNamespace returns the staging namespace for parentPath, holding one
// registry reference per Local until Close.
func (s *Local) tempNamespace(ctx context.Context, registry LocalTempNamespaceRegistry, parent *os.Root, parentPath string) (LocalTempNamespace, error) {
	d := &s.durability
	d.mu.Lock()
	namespace, held := d.namespaces[parentPath]
	d.mu.Unlock()
	if held {
		return namespace, nil
	}
	// Writers of one Local prepare each namespace once, so it holds one reference.
	d.namespaceMu.Lock()
	defer d.namespaceMu.Unlock()
	d.mu.Lock()
	namespace, held = d.namespaces[parentPath]
	d.mu.Unlock()
	if held {
		return namespace, nil
	}
	namespace, err := registry.PrepareNamespace(ctx, parent, parentPath)
	if err != nil {
		return namespace, err
	}
	d.mu.Lock()
	if d.namespaces == nil {
		d.namespaces = make(map[string]LocalTempNamespace)
	}
	d.namespaces[parentPath] = namespace
	d.mu.Unlock()
	return namespace, nil
}

// Close finishes deferred durability work, releases staging namespaces and
// closes the root. No Write may be in progress.
func (s *Local) Close() error {
	err := s.Sync(context.Background())
	d := &s.durability
	d.mu.Lock()
	namespaces, failed := d.namespaces, d.failed
	d.namespaces = nil
	d.mu.Unlock()
	if releaser, ok := s.tempRegistry.(LocalTempNamespaceReleaser); ok && !failed {
		for _, namespace := range namespaces {
			err = errors.Join(err, releaser.ReleaseNamespace(context.Background(), namespace))
		}
	}
	return errors.Join(err, s.root.Close())
}
