package transferstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
)

type Local struct {
	root         *os.Root
	scope        string
	rootPath     string
	tempRegistry LocalTempRegistry
	durability   localDurability
}

// NewLocal opens an existing directory. It never creates the storage root.
// os.Root confines filesystem operations even if a path component is replaced
// with a symlink concurrently. Directory bind mounts remain an operator concern.
func NewLocal(root string) (*Local, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	info, err := r.Stat(".")
	if err != nil {
		_ = r.Close()
		return nil, err
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		_ = r.Close()
		return nil, err
	}
	// Listing cursors survive reopening and directory mtime changes.
	identity, err := storageIdentity(r, ".", info)
	if err != nil {
		_ = r.Close()
		return nil, err
	}
	return &Local{root: r, scope: absolute + identity, rootPath: absolute}, nil
}

func localKey(key string, allowDirectory bool) (string, error) {
	if allowDirectory {
		key = strings.TrimSuffix(key, "/")
	}
	if key == "" || key == "." || strings.ContainsAny(key, "\\\x00") ||
		strings.HasPrefix(key, "/") || path.Clean(key) != key {
		return "", ErrInvalidKey
	}
	for _, component := range strings.Split(key, "/") {
		if component == ".." || component == "." || component == "" {
			return "", ErrInvalidKey
		}
	}
	if err := platformKey(key); err != nil {
		return "", err
	}
	return key, nil
}

// fingerprint uses only stable stat fields. Access time is deliberately excluded.
func fileIdentity(info fs.FileInfo) string {
	identity := ""
	value := reflect.ValueOf(info.Sys())
	if value.IsValid() && value.Kind() == reflect.Pointer {
		value = value.Elem()
	}
	if value.IsValid() && value.Kind() == reflect.Struct {
		for _, field := range []string{"Dev", "Ino"} {
			v := value.FieldByName(field)
			if v.IsValid() && v.CanUint() {
				identity += fmt.Sprintf("/%d", v.Uint())
			} else if v.IsValid() && v.CanInt() {
				identity += fmt.Sprintf("/%d", v.Int())
			}
		}
	}
	return identity
}

func fingerprint(info fs.FileInfo) string {
	return identityFingerprint(info, fileIdentity(info))
}

func identityFingerprint(info fs.FileInfo, identity string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s/%d/%d/%d", identity, info.Size(), info.ModTime().UnixNano(), info.Mode())))
	return "local:" + hex.EncodeToString(sum[:])
}

func localObject(key string, info fs.FileInfo) Object {
	return Object{Key: key, Size: info.Size(), ModifiedAt: info.ModTime(), ETag: fingerprint(info)}
}

func localError(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	return err
}

func (s *Local) Stat(ctx context.Context, key string) (Object, error) {
	if err := ctx.Err(); err != nil {
		return Object{}, err
	}
	name, err := localKey(key, true)
	if err != nil {
		return Object{}, err
	}
	info, err := s.root.Lstat(name)
	if err != nil {
		return Object{}, localError(err)
	}
	if strings.HasSuffix(key, "/") && info.IsDir() {
		object, err := storageObject(s.root, name, info)
		object.Key, object.Size = key, 0
		return object, err
	}
	if !info.Mode().IsRegular() {
		return Object{}, fmt.Errorf("%w: source is not a regular file", ErrInvalidKey)
	}
	return storageObject(s.root, key, info)
}

// walkCompare matches WalkDir's sorted, depth-first ordering. Raw string
// comparison would incorrectly order a/child after a.txt and lose resume items.
func walkCompare(left, right string) int {
	a, b := strings.Split(left, "/"), strings.Split(right, "/")
	for i := 0; i < len(a) && i < len(b); i++ {
		if difference := strings.Compare(a[i], b[i]); difference != 0 {
			return difference
		}
	}
	return len(a) - len(b)
}

func (s *Local) List(ctx context.Context, options ListOptions) (Page, error) {
	limit, err := pageLimit(options.Limit)
	if err != nil {
		return Page{}, err
	}
	if options.Prefix != "" {
		if _, err := localKey(options.Prefix, true); err != nil {
			return Page{}, err
		}
	}
	after, err := readCursor(options.Cursor, "local", s.scope, options.Prefix, options.StartAfter)
	if err != nil {
		return Page{}, err
	}
	if after != "" {
		if _, err := localKey(after, false); err != nil {
			return Page{}, ErrInvalidCursor
		}
	}
	page := Page{Objects: make([]Object, 0, limit)}
	err = fs.WalkDir(s.root.FS(), ".", func(key string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			if key != "." && vanished(walkErr) {
				if entry != nil && entry.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			return walkErr
		}
		// Symlinks and special files are not migration objects, as in the legacy scanner.
		if key != "." && entry.IsDir() && isLocalTempNamespace(s.root, key) {
			return fs.SkipDir
		}
		if entry.IsDir() || !entry.Type().IsRegular() || isNamespaceInternal(key) || !strings.HasPrefix(key, options.Prefix) ||
			(options.StartAfter != "" && key <= options.StartAfter) ||
			(after != "" && walkCompare(key, after) <= 0) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if vanished(err) {
				return nil
			}
			return err
		}
		probe, err := isSharedProbeFile(ctx, s.root, key, info)
		if vanished(err) {
			return nil
		}
		if err != nil || probe {
			return err
		}
		if len(page.Objects) == limit {
			page.NextCursor = makeCursor("local", s.scope, options.Prefix, options.StartAfter, page.Objects[limit-1].Key)
			return fs.SkipAll
		}
		object, err := storageObject(s.root, key, info)
		if vanished(err) {
			return nil
		}
		if err != nil {
			return err
		}
		page.Objects = append(page.Objects, object)
		return nil
	})
	if err != nil {
		return Page{}, err
	}
	return page, nil
}

// Scan performs one sorted depth-first walk. List is intended for interactive
// browsing, while this method keeps discovery linear for large local trees.
func (s *Local) Scan(ctx context.Context, options ListOptions, emit func(Object) error) error {
	if emit == nil {
		return errors.New("scan callback is required")
	}
	if options.Prefix != "" {
		if _, err := localKey(options.Prefix, true); err != nil {
			return err
		}
	}
	after, err := readCursor(options.Cursor, "local", s.scope, options.Prefix, options.StartAfter)
	if err != nil {
		return err
	}
	return fs.WalkDir(s.root.FS(), ".", func(key string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			if key != "." && vanished(walkErr) {
				if entry != nil && entry.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			return walkErr
		}
		if key != "." && entry.IsDir() && isLocalTempNamespace(s.root, key) {
			return fs.SkipDir
		}
		if entry.IsDir() || !entry.Type().IsRegular() || isNamespaceInternal(key) || !strings.HasPrefix(key, options.Prefix) ||
			(options.StartAfter != "" && key <= options.StartAfter) || (after != "" && walkCompare(key, after) <= 0) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if vanished(err) {
				return nil
			}
			return err
		}
		probe, err := isSharedProbeFile(ctx, s.root, key, info)
		if vanished(err) {
			return nil
		}
		if err != nil || probe {
			return err
		}
		object, err := storageObject(s.root, key, info)
		if vanished(err) {
			return nil
		}
		if err != nil {
			return err
		}
		return emit(object)
	})
}

func (s *Local) Open(ctx context.Context, key string, options OpenOptions) (io.ReadCloser, Object, error) {
	file, info, object, err := s.openPinned(ctx, key, options)
	if err != nil {
		return nil, Object{}, err
	}
	reader := cancellable(ctx, file, func() error { return verifyPinned(key, file, info, object) })
	return reader, object, nil
}

// OpenRanges serves concurrent ReadAt ranges from one pinned handle. Each range
// repeats Open's EOF identity check when it finishes, so a change during the
// copy fails the range that observes it.
func (s *Local) OpenRanges(ctx context.Context, key string, options OpenOptions) (RangeSource, Object, error) {
	file, info, object, err := s.openPinned(ctx, key, options)
	if err != nil {
		return nil, Object{}, err
	}
	return &localRanges{key: key, file: file, info: info, object: object}, object, nil
}

func (s *Local) openPinned(ctx context.Context, key string, options OpenOptions) (*os.File, os.FileInfo, Object, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, Object{}, err
	}
	name, err := localKey(key, false)
	if err != nil {
		return nil, nil, Object{}, err
	}
	info, err := s.root.Lstat(name)
	if err != nil {
		return nil, nil, Object{}, localError(err)
	}
	if !info.Mode().IsRegular() {
		return nil, nil, Object{}, fmt.Errorf("%w: source is not a regular file", ErrInvalidKey)
	}
	file, err := s.root.Open(name)
	if err != nil {
		return nil, nil, Object{}, localError(err)
	}
	beforeOpen := info
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		if err == nil {
			err = ErrInvalidKey
		}
		return nil, nil, Object{}, err
	}
	if !os.SameFile(beforeOpen, info) {
		_ = file.Close()
		return nil, nil, Object{}, ErrSourceChanged
	}
	object, err := openedObject(key, file, info)
	if err != nil {
		_ = file.Close()
		return nil, nil, Object{}, err
	}
	if err := checkExpected(object, options.Expected); err != nil {
		_ = file.Close()
		return nil, nil, Object{}, err
	}
	return file, info, object, nil
}

func verifyPinned(key string, file *os.File, info os.FileInfo, object Object) error {
	latest, err := file.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(info, latest) {
		return ErrSourceChanged
	}
	actual, err := openedObject(key, file, latest)
	if err != nil {
		return err
	}
	return checkExpected(actual, &object)
}

type localRanges struct {
	key    string
	file   *os.File
	info   os.FileInfo
	object Object
}

func (r *localRanges) Close() error { return r.file.Close() }

func (r *localRanges) OpenRange(ctx context.Context, offset, length int64) (io.ReadCloser, error) {
	if offset < 0 || length <= 0 || offset > r.object.Size-length {
		return nil, errors.New("source range is outside the object")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Closing a range must not close the shared handle.
	section := io.NopCloser(io.NewSectionReader(r.file, offset, length))
	return cancellable(ctx, section, func() error { return verifyPinned(r.key, r.file, r.info, r.object) }), nil
}

func (s *Local) Write(ctx context.Context, key string, input io.Reader, options WriteOptions) (result WriteResult, err error) {
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if options.Size < 0 || input == nil {
		return result, fmt.Errorf("nonnegative size and input are required")
	}
	name, err := localKey(key, true)
	if err != nil {
		return result, err
	}
	if reservedKeyComponent(name) {
		return result, fmt.Errorf("%w: key uses a reserved staging name", ErrInvalidKey)
	}
	stop := interruptInput(ctx, input)
	defer stop()
	reader := contextReader{ctx, input}
	if strings.HasSuffix(key, "/") {
		if options.Size != 0 {
			return result, fmt.Errorf("%w: directory marker must be empty", ErrSizeMismatch)
		}
		if err := requireEOF(reader); err != nil {
			return result, err
		}
		if err := s.root.MkdirAll(name, 0o755); err != nil {
			return result, err
		}
		result.Object, err = s.Stat(ctx, key)
		result.Committed = err == nil
		if err == nil && options.DeferDurability {
			err = s.DeferExisting(ctx, key)
		}
		return result, err
	}
	deferred := options.DeferDurability && deferredDurabilitySupported
	parent, base := path.Dir(name), path.Base(name)
	entry, err := s.directory(parent, true)
	if err != nil {
		return result, err
	}
	defer s.release(entry)
	// skipped reports an existing destination. A deferred caller commits the
	// skip after its next Sync, which then covers this directory entry too.
	skipped := func(prior Object, statErr error) (WriteResult, error) {
		if statErr == nil && deferred {
			s.markDirty(entry, dirtyDestination, "")
		}
		return WriteResult{Object: prior, Skipped: statErr == nil}, statErr
	}
	// The common case, an absent destination, needs one lookup in the cached
	// directory; anything else takes the full Stat for its exact semantics.
	if _, lookupErr := entry.root.Lstat(base); !errors.Is(lookupErr, fs.ErrNotExist) {
		if prior, statErr := s.Stat(ctx, key); statErr == nil {
			if !options.Overwrite {
				return skipped(prior, nil)
			}
		} else if !errors.Is(statErr, ErrNotFound) {
			return result, statErr
		}
	}
	file, temp, err := s.tempFile(ctx, entry)
	if err != nil {
		// A refused reservation keeps its record; recovery decides about it.
		return result, err
	}
	temporary := temp.name
	registered := s.tempRegistry != nil
	handedOff, tempGone := false, false
	defer func() {
		_ = file.Close()
		if !tempGone {
			if _, removeErr := removeLocalTempEntry(context.WithoutCancel(ctx), entry.temp, temporary, temp.identity, false); removeErr != nil {
				err = errors.Join(err, fmt.Errorf("remove temporary file: %w", removeErr))
				return // Keep its durable entry if cleanup did not succeed.
			}
		}
		if registered && !handedOff {
			if syncErr := syncTempNamespace(entry.temp); syncErr != nil {
				err = errors.Join(err, fmt.Errorf("sync temporary cleanup: %w", syncErr))
				return // Keep the journal until removal is durable.
			}
			if forgetErr := s.tempRegistry.Forget(context.WithoutCancel(ctx), temp.id); forgetErr != nil {
				err = errors.Join(err, fmt.Errorf("clear temporary file record: %w", forgetErr))
			}
		}
	}()
	written, err := io.CopyBuffer(file, io.LimitReader(reader, options.Size), make([]byte, 256*1024))
	if err != nil {
		return result, err
	}
	if written != options.Size {
		return result, fmt.Errorf("%w: expected %d, read %d", ErrSizeMismatch, options.Size, written)
	}
	if err := requireEOF(reader); err != nil {
		return result, err
	}
	// A published file must be complete whenever it is visible: SKIP treats
	// any existing destination as done, including after a crash.
	if deferred {
		err = syncPayloadOrdered(file, &s.durability.ordering)
	} else {
		err = file.Sync()
	}
	if err != nil {
		return result, err
	}
	info, err := file.Stat()
	if err != nil {
		return result, err
	}
	object, err := openedObject(key, file, info)
	if err != nil {
		return result, err
	}
	publicationIdentity, err := openedIdentity(file, info)
	if err != nil {
		return result, err
	}
	if err := file.Close(); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	tempHandle := entry.handle
	if entry.tempFile != nil {
		tempHandle = entry.tempFile
	}
	// A hard link publishes a complete file without replacing a target created
	// concurrently. Rename alone would race SKIP on Unix.
	err = publishTempAcrossDirs(entry.temp, tempHandle, entry.handle, temporary, base, options.Overwrite, publicationIdentity)
	if !options.Overwrite && errors.Is(err, fs.ErrExist) {
		return skipped(s.Stat(ctx, key))
	}
	if err != nil {
		return result, err
	}
	tempGone = options.Overwrite // The rename consumed the temporary name.
	result = WriteResult{Object: object, Committed: true}
	if !options.Overwrite {
		if _, err := removeLocalTempEntry(context.WithoutCancel(ctx), entry.temp, temporary, temp.identity, false); err != nil {
			return result, fmt.Errorf("object committed but temporary link removal failed: %w", err)
		}
		tempGone = true
	}
	if deferred {
		s.markDirty(entry, dirtyDestination|dirtyStaging, temp.id)
		handedOff = true
		return result, nil
	}
	if err := syncPublished(entry.root, base); err != nil {
		return result, fmt.Errorf("object committed but publication sync failed: %w", err)
	}
	return result, nil
}
