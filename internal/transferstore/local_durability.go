package transferstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
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

// LocalTempBatchRegistrar records many temporary files in one durable commit.
// Registries without it receive one Register call per file.
type LocalTempBatchRegistrar interface {
	RegisterBatch(context.Context, []LocalTempRecord) error
}

// LocalTempNamespaceReleaser is implemented by namespace registries that
// remove a staging directory once nobody uses it. Every successful
// PrepareNamespace holds one reference, which Local releases on Close.
type LocalTempNamespaceReleaser interface {
	ReleaseNamespace(context.Context, LocalTempNamespace) error
}

const (
	// maxTempReserve bounds how many empty temporary files one destination
	// directory registers ahead of use. Reservations double from one, so a
	// directory that receives a single object registers a single file.
	maxTempReserve = 16
	// maxCachedDirs bounds the idle directory entries a Local keeps open.
	// Each holds up to four descriptors; entries in use are never evicted.
	// Evicted entries that still owe a directory fsync stay open until a
	// Sync covers them; past maxCachedDirs of those, Write runs that Sync.
	maxCachedDirs = 8
)

// localDir caches one resolved destination directory. Every use first
// re-resolves the path through the root and compares the directory identity,
// so a replaced or moved directory is never written through a stale handle;
// the check costs one path walk instead of the MkdirAll, OpenRoot, namespace
// validation and directory opens it replaces.
//
// users, lastUse, retired and closed are guarded by localDurability.mu. An
// entry is closed only once it is retired (evicted or replaced), unused and
// has nothing left for a Sync to fsync.
type localDir struct {
	parent string // slash path below the root
	info   fs.FileInfo
	root   *os.Root // pinned destination directory
	handle *os.File // same directory: publication target and fsync handle

	users   int
	lastUse uint64
	retired bool
	closed  bool

	setup     sync.Mutex // guards the namespace fields and the reserve
	staged    bool       // namespace fields are set
	namespace *LocalTempNamespace
	temp      *os.Root // staging namespace, or root without a namespace registry
	tempInfo  fs.FileInfo
	tempPath  string // absolute path of temp, the records' ParentPath
	tempIdent string
	tempFile  *os.File // temp's handle, when it differs from handle
	reserve   []reservedTemp
	nextBatch int
}

// reservedTemp names an empty, registered temporary file not yet used. It is
// reopened by name and identity when used, so reservations hold no descriptor.
type reservedTemp struct {
	name, id string
	identity string
}

// orderingCache remembers per filesystem whether orderedFileSync may skip
// the full fsync. It lives as long as one Local.
type orderingCache struct{ modes sync.Map }

// localDurability holds the directory cache and the work that deferred writes
// still owe.
//
// A deferred write orders its payload before publication (orderedFileSync),
// publishes, removes its temporary name and returns. Sync then fsyncs every
// directory those writes changed (the destination directory, and the staging
// directory the temporary name was removed from), and only after that clears
// the temporary-file records. Until a record is cleared, restart recovery
// still owns the temporary name, so a crash at any point leaves either a
// complete published object or a registered temporary file to remove.
type localDurability struct {
	ordering    orderingCache
	syncMu      sync.Mutex // one Sync at a time, so a returning Sync covers all earlier writes
	namespaceMu sync.Mutex // serializes PrepareNamespace calls
	openMu      sync.Mutex // serializes cache misses, so each directory is opened once
	mu          sync.Mutex
	dirs        map[string]*localDir // current cache, by parent path
	open        map[*localDir]bool   // every entry not yet closed
	clock       uint64
	dirty       map[*localDir]dirtyDirs
	forget      []string // temporary-file record IDs to clear after the fsyncs
	failed      error    // a failed Sync; every later Sync reports it
	namespaces  map[string]LocalTempNamespace
}

// dirtyDirs says which directories of an entry a Sync must fsync.
type dirtyDirs uint8

const (
	dirtyDestination dirtyDirs = 1 << iota
	dirtyStaging
)

// directory returns the cached, revalidated entry for parent, creating the
// directory when create is set. The caller must release the entry.
func (s *Local) directory(parent string, create bool) (*localDir, error) {
	d := &s.durability
	lookup := func() (*localDir, error) {
		d.mu.Lock()
		entry := d.dirs[parent]
		if entry != nil {
			entry.users++
			d.clock++
			entry.lastUse = d.clock
		}
		d.mu.Unlock()
		if entry == nil {
			return nil, nil
		}
		info, err := s.root.Stat(parent)
		if err == nil && info.IsDir() && os.SameFile(info, entry.info) {
			return entry, nil
		}
		// Replaced or removed: retire it and resolve again. A pending Sync
		// still fsyncs it before it is closed.
		d.mu.Lock()
		s.retire(entry)
		d.mu.Unlock()
		s.release(entry)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		return nil, nil
	}
	if entry, err := lookup(); entry != nil || err != nil {
		return entry, err
	}
	d.openMu.Lock()
	defer d.openMu.Unlock()
	if entry, err := lookup(); entry != nil || err != nil {
		return entry, err
	}
	if create {
		if err := s.root.MkdirAll(parent, 0o755); err != nil {
			return nil, err
		}
	}
	root, err := s.root.OpenRoot(parent)
	if err != nil {
		return nil, err
	}
	handle, err := root.Open(".")
	if err != nil {
		root.Close()
		return nil, err
	}
	info, err := handle.Stat()
	if err != nil {
		handle.Close()
		root.Close()
		return nil, err
	}
	entry := &localDir{parent: parent, info: info, root: root, handle: handle, users: 1}
	d.mu.Lock()
	if d.dirs == nil {
		d.dirs, d.open = make(map[string]*localDir), make(map[*localDir]bool)
	}
	d.clock++
	entry.lastUse = d.clock
	d.dirs[parent] = entry
	d.open[entry] = true
	victims := s.evictionCandidates()
	d.mu.Unlock()
	// A clean victim closes now. One that owes a directory fsync stays open
	// until a Sync covers it: every fsync and its outcome belong to Sync, so
	// no barrier can report success for a directory whose fsync failed.
	for _, victim := range victims {
		s.release(victim)
	}
	return entry, nil
}

// evictionCandidates retires the least recently used idle entries beyond
// maxCachedDirs and returns them with an extra user held, which the caller
// releases. The caller holds d.mu.
func (s *Local) evictionCandidates() []*localDir {
	d := &s.durability
	var victims []*localDir
	for len(d.dirs) > maxCachedDirs {
		var oldest *localDir
		for _, entry := range d.dirs {
			if entry.users == 0 && (oldest == nil || entry.lastUse < oldest.lastUse) {
				oldest = entry
			}
		}
		if oldest == nil {
			break // Every entry is in use; they are released later.
		}
		oldest.users++
		s.retire(oldest)
		victims = append(victims, oldest)
	}
	return victims
}

// tooManyRetired reports that evicted entries waiting for a Sync hold more
// descriptors than the cache itself may.
func (s *Local) tooManyRetired() bool {
	d := &s.durability
	d.mu.Lock()
	defer d.mu.Unlock()
	retired := 0
	for entry := range d.open {
		if entry.retired {
			retired++
		}
	}
	return retired > maxCachedDirs
}

// retire removes entry from the cache and closes it if possible. The caller
// holds d.mu.
func (s *Local) retire(entry *localDir) {
	d := &s.durability
	if d.dirs[entry.parent] == entry {
		delete(d.dirs, entry.parent)
	}
	entry.retired = true
}

// release drops one user of entry and closes it once it is retired, unused
// and clean.
func (s *Local) release(entry *localDir) {
	d := &s.durability
	d.mu.Lock()
	entry.users--
	closeIt := entry.retired && entry.users == 0 && d.dirty[entry] == 0 && !entry.closed
	if closeIt {
		entry.closed = true
		delete(d.open, entry)
	}
	d.mu.Unlock()
	if closeIt {
		s.closeEntry(entry)
	}
}

// closeEntry removes the entry's unused reservations and closes its
// descriptors. An unused reservation is empty: if its removal is lost in a
// crash, recovery removes the empty unregistered file anyway, so its record
// is cleared by the next Sync without another staging-directory fsync.
func (s *Local) closeEntry(entry *localDir) error {
	var err error
	entry.setup.Lock()
	var forget []string
	for _, temp := range entry.reserve {
		if _, removeErr := removeLocalTempEntry(context.Background(), entry.temp, temp.name, temp.identity, true); removeErr != nil {
			err = errors.Join(err, removeErr)
			continue // Keep its record; recovery handles it.
		}
		forget = append(forget, temp.id)
	}
	entry.reserve = nil
	if entry.tempFile != nil {
		entry.tempFile.Close()
	}
	if entry.temp != nil && entry.temp != entry.root {
		entry.temp.Close()
	}
	entry.handle.Close()
	entry.root.Close()
	entry.setup.Unlock()
	if len(forget) > 0 {
		s.durability.mu.Lock()
		s.durability.forget = append(s.durability.forget, forget...)
		s.durability.mu.Unlock()
	}
	return err
}

// staging returns the directory that holds entry's temporary files, opening
// and validating the registry namespace once per entry. Later uses compare
// only the namespace identity. The caller holds entry.setup.
func (s *Local) staging(ctx context.Context, entry *localDir) error {
	if entry.staged {
		if entry.namespace == nil {
			return nil
		}
		info, err := entry.root.Lstat(entry.namespace.Name)
		if err != nil {
			return err
		}
		if !info.IsDir() || !os.SameFile(info, entry.tempInfo) {
			return ErrTempIdentity
		}
		return nil
	}
	entry.temp, entry.tempInfo = entry.root, entry.info
	entry.tempPath = filepath.Join(s.rootPath, filepath.FromSlash(entry.parent))
	registry, ok := s.tempRegistry.(LocalTempNamespaceRegistry)
	if ok {
		namespace, err := s.tempNamespace(ctx, registry, entry.root, entry.tempPath)
		if err != nil {
			return err
		}
		temp, err := OpenLocalTempNamespace(entry.root, namespace)
		if err != nil {
			return err
		}
		handle, err := temp.Open(".")
		if err != nil {
			temp.Close()
			return err
		}
		info, err := handle.Stat()
		if err != nil {
			handle.Close()
			temp.Close()
			return err
		}
		entry.namespace, entry.temp, entry.tempFile, entry.tempInfo = &namespace, temp, handle, info
		entry.tempPath = filepath.Join(entry.tempPath, namespace.Name)
	}
	if s.tempRegistry != nil {
		identity, err := storageIdentity(entry.temp, ".", entry.tempInfo)
		if err != nil {
			return err
		}
		entry.tempIdent = identity
	}
	entry.staged = true
	return nil
}

// tempFile returns a new, empty temporary file in entry's staging directory,
// open for writing. With a registry its record is durable before the caller
// writes any byte; a crash then leaves a registered file that restart
// recovery removes.
func (s *Local) tempFile(ctx context.Context, entry *localDir) (*os.File, reservedTemp, error) {
	entry.setup.Lock()
	defer entry.setup.Unlock()
	if err := s.staging(ctx, entry); err != nil {
		return nil, reservedTemp{}, err
	}
	if s.tempRegistry == nil {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return nil, reservedTemp{}, err
		}
		return createTemp(entry.temp, ".oss-transfer-"+hex.EncodeToString(nonce[:])+".tmp", "")
	}
	if !validHex(s.tempRegistry.OwnerID(), 32) {
		return nil, reservedTemp{}, ErrTempIdentity
	}
	if len(entry.reserve) == 0 {
		if err := s.reserveTemps(ctx, entry); err != nil {
			return nil, reservedTemp{}, err
		}
	}
	temp := entry.reserve[0]
	entry.reserve = entry.reserve[1:]
	file, err := openReserved(entry.temp, temp)
	return file, temp, err
}

// openReserved reopens a reservation, refusing a replaced or nonempty file.
// On refusal the record stays, so the caller's cleanup and recovery keep
// treating the name as the original file.
func openReserved(directory *os.Root, temp reservedTemp) (*os.File, error) {
	file, err := directory.OpenFile(temp.name, os.O_WRONLY, 0)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err == nil {
		var identity string
		identity, err = openedIdentity(file, info)
		if err == nil && (identity != temp.identity || !info.Mode().IsRegular() || info.Size() != 0) {
			err = ErrTempIdentity
		}
	}
	if err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

// reserveTemps creates a batch of empty temporary files and registers them in
// one commit. Only a staging namespace gets more than one at a time.
func (s *Local) reserveTemps(ctx context.Context, entry *localDir) error {
	count := 1
	if entry.namespace != nil {
		count = max(1, entry.nextBatch)
		entry.nextBatch = min(2*count, maxTempReserve)
	}
	owner := s.tempRegistry.OwnerID()
	temps := make([]reservedTemp, 0, count)
	records := make([]LocalTempRecord, 0, count)
	discard := func() {
		for _, temp := range temps {
			// Unregistered and empty: safe to remove; recovery would too.
			_, _ = removeLocalTempEntry(context.WithoutCancel(ctx), entry.temp, temp.name, temp.identity, true)
		}
	}
	for range count {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			discard()
			return err
		}
		id := hex.EncodeToString(nonce[:])
		file, temp, err := createTemp(entry.temp, trackedTempName(owner, id), id)
		if err != nil {
			discard()
			return err
		}
		file.Close()
		temps = append(temps, temp)
		records = append(records, LocalTempRecord{Owner: owner, ID: id, ParentPath: entry.tempPath, ParentIdentity: entry.tempIdent, Name: temp.name, FileIdentity: temp.identity})
	}
	// Registration must commit before any input byte is written. A crash
	// before this barrier leaves empty files inside the durably registered
	// private namespace, which startup recovery can safely remove.
	var err error
	if batch, ok := s.tempRegistry.(LocalTempBatchRegistrar); ok {
		err = batch.RegisterBatch(ctx, records)
	} else {
		for i, record := range records {
			if err = s.tempRegistry.Register(ctx, record); err != nil {
				// discard removes the registered files too; clear their records later.
				for _, done := range records[:i] {
					s.markDirty(entry, dirtyStaging, done.ID)
				}
				break
			}
		}
	}
	if err != nil {
		discard()
		return err
	}
	entry.reserve = append(entry.reserve, temps...)
	return nil
}

func createTemp(directory *os.Root, name, id string) (*os.File, reservedTemp, error) {
	file, err := directory.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, reservedTemp{}, err
	}
	info, err := file.Stat()
	if err == nil {
		var identity string
		if identity, err = openedIdentity(file, info); err == nil {
			return file, reservedTemp{name: name, id: id, identity: identity}, nil
		}
	}
	file.Close()
	_ = directory.Remove(name)
	return nil, reservedTemp{}, err
}

// markDirty records which directories of entry changed, and the
// temporary-file record to clear once they are durable.
func (s *Local) markDirty(entry *localDir, dirs dirtyDirs, id string) {
	d := &s.durability
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dirty == nil {
		d.dirty = make(map[*localDir]dirtyDirs)
	}
	d.dirty[entry] |= dirs
	if id != "" {
		d.forget = append(d.forget, id)
	}
}

// DeferExisting makes the next Sync cover the directory entry of an object
// this Local did not write, such as a destination a SKIP result relies on.
func (s *Local) DeferExisting(_ context.Context, key string) error {
	if !deferredDurabilitySupported {
		return nil
	}
	name, err := localKey(key, true)
	if err != nil {
		return err
	}
	entry, err := s.directory(path.Dir(name), false)
	if err != nil {
		return err
	}
	s.markDirty(entry, dirtyDestination, "")
	s.release(entry)
	return nil
}

// syncEntry fsyncs the directories of entry named by dirs.
func syncEntry(entry *localDir, dirs dirtyDirs) error {
	staging := entry.tempFile
	if staging == nil {
		// Without a namespace the staging directory is the destination.
		staging = entry.handle
		if dirs&dirtyStaging != 0 {
			dirs = dirtyDestination
		}
	}
	var err error
	if dirs&dirtyDestination != 0 {
		err = errors.Join(err, syncDirectory(entry.handle))
	}
	if dirs&dirtyStaging != 0 {
		err = errors.Join(err, syncDirectory(staging))
	}
	return err
}

// Sync makes every deferred write that returned before the call durable, then
// clears their temporary-file records in one registry commit. After a failure
// it keeps failing, so no later result can be committed on top of writes
// whose durability is unknown. Retired entries are closed once synced.
func (s *Local) Sync(ctx context.Context) error {
	d := &s.durability
	d.syncMu.Lock()
	defer d.syncMu.Unlock()
	d.mu.Lock()
	if d.failed != nil {
		d.mu.Unlock()
		return d.failed
	}
	dirty, ids := d.dirty, d.forget
	d.dirty, d.forget = nil, nil
	for entry := range dirty {
		entry.users++ // Keep it open while it is synced.
	}
	d.mu.Unlock()
	var err error
	for entry, dirs := range dirty {
		err = errors.Join(err, syncEntry(entry, dirs))
	}
	for entry := range dirty {
		s.release(entry)
	}
	if err == nil && len(ids) > 0 {
		ctx = context.WithoutCancel(ctx)
		if batch, ok := s.tempRegistry.(LocalTempBatchForgetter); ok {
			err = batch.ForgetBatch(ctx, ids)
		} else {
			for _, id := range ids {
				err = errors.Join(err, s.tempRegistry.Forget(ctx, id))
			}
		}
	}
	if err != nil {
		// Records that were not cleared stay; restart recovery removes
		// whatever temporary names reappear.
		err = fmt.Errorf("sync local destination: %w", err)
		d.mu.Lock()
		d.failed = err
		d.mu.Unlock()
	}
	return err
}

// tempNamespace returns the staging namespace for parentPath, holding one
// registry reference per Local until Close.
func (s *Local) tempNamespace(ctx context.Context, registry LocalTempNamespaceRegistry, parent *os.Root, parentPath string) (LocalTempNamespace, error) {
	d := &s.durability
	// Writers of one Local prepare each namespace once, so it holds one reference.
	d.namespaceMu.Lock()
	defer d.namespaceMu.Unlock()
	d.mu.Lock()
	namespace, held := d.namespaces[parentPath]
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

// Close removes unused reserved temporary files, finishes deferred durability
// work, closes cached directories, releases staging namespaces and closes the
// root. No Write may be in progress.
func (s *Local) Close() error {
	d := &s.durability
	d.mu.Lock()
	var entries []*localDir
	for entry := range d.open {
		entries = append(entries, entry)
	}
	d.mu.Unlock()
	// Remove unused reservations first, so their records go with this Sync.
	var err error
	for _, entry := range entries {
		entry.setup.Lock()
		reserve := entry.reserve
		entry.reserve = nil
		entry.setup.Unlock()
		for _, temp := range reserve {
			if _, removeErr := removeLocalTempEntry(context.Background(), entry.temp, temp.name, temp.identity, true); removeErr != nil {
				err = errors.Join(err, removeErr)
				continue
			}
			d.mu.Lock()
			d.forget = append(d.forget, temp.id)
			d.mu.Unlock()
		}
	}
	err = errors.Join(err, s.Sync(context.Background()))
	d.mu.Lock()
	namespaces := d.namespaces
	entries = entries[:0]
	for entry := range d.open {
		entries = append(entries, entry)
	}
	d.namespaces, d.dirs, d.open, d.dirty = nil, nil, nil, nil
	d.mu.Unlock()
	for _, entry := range entries {
		err = errors.Join(err, s.closeEntry(entry))
	}
	// The registry keeps any namespace a surviving temporary-file record
	// still points into, so release unconditionally.
	if releaser, ok := s.tempRegistry.(LocalTempNamespaceReleaser); ok {
		for _, namespace := range namespaces {
			err = errors.Join(err, releaser.ReleaseNamespace(context.Background(), namespace))
		}
	}
	return errors.Join(err, s.root.Close())
}
