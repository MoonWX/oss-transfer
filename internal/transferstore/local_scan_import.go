package transferstore

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ImportScanCursor rebinds a cursor from another mount of a shared directory.
// The Master must verify its original shared-volume marker before requesting
// this operation. Directory inode identities still have to match; a marker at
// the root does not authorize skipping through a replaced child directory.
func (s *Local) ImportScanCursor(options ListOptions, sourceRoot string) (string, error) {
	if options.Cursor == "" {
		return "", nil
	}
	if len(options.Cursor) > 32768 {
		return "", ErrInvalidCursor
	}
	data, err := base64.RawURLEncoding.DecodeString(options.Cursor)
	if err != nil {
		return "", ErrInvalidCursor
	}
	var saved cursor
	if json.Unmarshal(data, &saved) != nil || saved.Kind != localScanCursorKind ||
		saved.Prefix != options.Prefix || saved.StartAfter != options.StartAfter || saved.Position == "" || sourceRoot == "" {
		return "", ErrInvalidCursor
	}
	root, err := filepath.Abs(sourceRoot)
	if err != nil || !strings.HasPrefix(saved.Scope, root+"/") {
		return "", ErrInvalidCursor
	}
	// Reuse the ordinary parser for path/stack validation, with the new scope.
	options.Cursor = makeCursor(localScanCursorKind, s.scope, options.Prefix, options.StartAfter, saved.Position)
	frames, err := s.scanFrames(options)
	if err != nil {
		return "", err
	}
	if saved.Scope != root+frames[0].Identity {
		return "", ErrInvalidCursor
	}
	for i := range frames {
		previous := frames[i].Identity
		before, err := s.root.Lstat(frames[i].key)
		if err != nil || !before.IsDir() {
			return "", fmt.Errorf("%w: shared scan directory %q is unavailable", ErrSourceChanged, frames[i].key)
		}
		file, err := s.root.Open(frames[i].key)
		if err != nil {
			return "", err
		}
		actual, err := file.Stat()
		if err == nil && (!actual.IsDir() || !os.SameFile(before, actual)) {
			err = ErrSourceChanged
		}
		if err == nil {
			frames[i].Identity, err = openedIdentity(file, actual)
		}
		closeErr := file.Close()
		if err != nil {
			return "", err
		}
		if closeErr != nil {
			return "", closeErr
		}
		if !sameSharedDirectoryIdentity(previous, frames[i].Identity) {
			return "", fmt.Errorf("%w: shared scan directory %q has a different identity", ErrSourceChanged, frames[i].key)
		}
	}
	return s.scanCursor(options, frames)
}

func sameSharedDirectoryIdentity(left, right string) bool {
	// Unix device IDs are local to a mount. The inode is supplied by the shared
	// filesystem and must match. Refuse placeholder/absent inode identities.
	l, r := strings.Split(left, "/"), strings.Split(right, "/")
	if len(l) == 3 && len(r) == 3 && l[0] == "" && r[0] == "" {
		li, le := strconv.ParseUint(l[2], 10, 64)
		ri, re := strconv.ParseUint(r[2], 10, 64)
		_, ld := strconv.ParseUint(l[1], 10, 64)
		_, rd := strconv.ParseUint(r[1], 10, 64)
		return le == nil && re == nil && ld == nil && rd == nil && li != 0 && li == ri
	}
	// Windows volume/file identities are not portable across mounts. Preserve
	// exact matching when both Agents expose the same stable file identity.
	if left == right && len(l) == 4 && l[0] == "" {
		_, ve := strconv.ParseUint(l[1], 10, 32)
		hi, he := strconv.ParseUint(l[2], 10, 32)
		lo, le := strconv.ParseUint(l[3], 10, 32)
		return ve == nil && he == nil && le == nil && (hi != 0 || lo != 0)
	}
	return false
}
