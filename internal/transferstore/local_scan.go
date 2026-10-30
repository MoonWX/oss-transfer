package transferstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
)

const localScanCursorKind = "local-scan-v1"

// Names and directory identities are sufficient to reopen only the active DFS
// path. Completed subtrees are never entered again. A very large active directory
// must still have its names enumerated and sorted on recovery, but previously
// consumed files are not stat'ed and no full directory inventory is persisted.
type localScanFrame struct {
	Name     string `json:"name,omitempty"`
	Identity string `json:"identity"`
	After    string `json:"after,omitempty"`

	key     string
	entries []fs.DirEntry
	next    int
}

func scanEntryName(name string) bool {
	if strings.Contains(name, "/") {
		return false
	}
	_, err := localKey(name, false)
	return err == nil
}

func (s *Local) scanFrames(options ListOptions) ([]localScanFrame, error) {
	position, err := readCursor(options.Cursor, localScanCursorKind, s.scope, options.Prefix, options.StartAfter)
	if err != nil {
		return nil, err
	}
	if position == "" {
		return []localScanFrame{{key: "."}}, nil
	}
	var frames []localScanFrame
	if json.Unmarshal([]byte(position), &frames) != nil || len(frames) == 0 || len(frames) > 1024 {
		return nil, ErrInvalidCursor
	}
	for index := range frames {
		frame := &frames[index]
		if frame.Identity == "" || (frame.After != "" && !scanEntryName(frame.After)) {
			return nil, ErrInvalidCursor
		}
		if index == 0 {
			if frame.Name != "" {
				return nil, ErrInvalidCursor
			}
			frame.key = "."
		} else {
			if !scanEntryName(frame.Name) || frames[index-1].After != frame.Name {
				return nil, ErrInvalidCursor
			}
			frame.key = path.Join(frames[index-1].key, frame.Name)
		}
	}
	return frames, nil
}

func (s *Local) loadScanFrame(frame *localScanFrame) error {
	before, err := s.root.Lstat(frame.key)
	if err != nil {
		if frame.Identity != "" && errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: scan directory %q is missing", ErrSourceChanged, frame.key)
		}
		return err
	}
	if !before.IsDir() {
		return fmt.Errorf("%w: scan directory %q was replaced", ErrSourceChanged, frame.key)
	}
	file, err := s.root.Open(frame.key)
	if err != nil {
		return err
	}
	defer file.Close()
	actual, err := file.Stat()
	if err != nil {
		return err
	}
	if !actual.IsDir() || !os.SameFile(before, actual) {
		return ErrSourceChanged
	}
	identity, err := openedIdentity(file, actual)
	if err != nil {
		return err
	}
	if frame.Identity != "" && frame.Identity != identity {
		return fmt.Errorf("%w: scan directory %q was replaced", ErrSourceChanged, frame.key)
	}
	frame.Identity = identity
	frame.entries, err = file.ReadDir(-1)
	if err != nil {
		return err
	}
	sort.Slice(frame.entries, func(i, j int) bool { return frame.entries[i].Name() < frame.entries[j].Name() })
	frame.next = sort.Search(len(frame.entries), func(index int) bool { return frame.entries[index].Name() > frame.After })
	return nil
}

func (s *Local) scanCursor(options ListOptions, frames []localScanFrame) (string, error) {
	if len(frames) == 0 {
		return "", nil
	}
	encoded, err := json.Marshal(frames)
	if err != nil {
		return "", err
	}
	value := makeCursor(localScanCursorKind, s.scope, options.Prefix, options.StartAfter, string(encoded))
	if len(value) > 32768 {
		return "", fmt.Errorf("%w: local directory checkpoint exceeds 32 KiB", ErrInvalidCursor)
	}
	return value, nil
}

// ScanPages returns resumable discovery pages. Its cursor is an opaque DFS
// stack, not the final object's key: directories and filtered entries must also
// advance the checkpoint. The caller must never persist a cursor ahead of its
// preceding objects; a batched writer may replay its uncommitted tail after a
// restart. An empty cursor identifies only the final page.
func (s *Local) ScanPages(ctx context.Context, options ListOptions, emit func(Page) error) error {
	if emit == nil {
		return errors.New("scan callback is required")
	}
	limit, err := pageLimit(options.Limit)
	if err != nil {
		return err
	}
	if options.Prefix != "" {
		if _, err := localKey(options.Prefix, true); err != nil {
			return err
		}
	}
	frames, err := s.scanFrames(options)
	if err != nil {
		return err
	}
	// Validate every directory on a resumed path before yielding any objects.
	for index := range frames {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.loadScanFrame(&frames[index]); err != nil {
			return err
		}
	}
	objects := make([]Object, 0, limit)
	visited := 0
	flush := func() error {
		for len(frames) > 0 && frames[len(frames)-1].next == len(frames[len(frames)-1].entries) {
			frames = frames[:len(frames)-1]
		}
		cursor, err := s.scanCursor(options, frames)
		if err != nil {
			return err
		}
		if err := emit(Page{Objects: objects, NextCursor: cursor}); err != nil {
			return err
		}
		objects = make([]Object, 0, limit)
		visited = 0
		return nil
	}
	for len(frames) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		frame := &frames[len(frames)-1]
		if frame.next == len(frame.entries) {
			frames = frames[:len(frames)-1]
			continue
		}
		entry := frame.entries[frame.next]
		if !scanEntryName(entry.Name()) {
			return ErrInvalidKey
		}
		key := path.Join(frame.key, entry.Name())
		// Advance only after the entry has been successfully inspected. On
		// callback failure the caller retains the preceding durable cursor.
		if entry.IsDir() {
			if !isLocalTempNamespace(s.root, key) &&
				(strings.HasPrefix(key+"/", options.Prefix) || strings.HasPrefix(options.Prefix, key+"/")) {
				child := localScanFrame{Name: entry.Name(), key: key}
				if !scanEntryName(child.Name) {
					return ErrInvalidKey
				}
				if err := s.loadScanFrame(&child); err != nil {
					return err
				}
				frame.After, frame.next = entry.Name(), frame.next+1
				frames = append(frames, child)
			} else {
				frame.After, frame.next = entry.Name(), frame.next+1
			}
		} else {
			if entry.Type().IsRegular() && strings.HasPrefix(key, options.Prefix) &&
				(options.StartAfter == "" || key > options.StartAfter) {
				info, err := s.root.Lstat(key)
				if err != nil {
					return err
				}
				if !info.Mode().IsRegular() {
					return fmt.Errorf("%w: scan file %q was replaced", ErrSourceChanged, key)
				}
				probe, err := isSharedProbeFile(ctx, s.root, key, info)
				if err != nil {
					return err
				}
				if !probe {
					object, err := storageObject(s.root, key, info)
					if err != nil {
						return err
					}
					objects = append(objects, object)
				}
			}
			frame.After, frame.next = entry.Name(), frame.next+1
		}
		visited++
		if visited >= limit {
			if err := flush(); err != nil {
				return err
			}
			if len(frames) == 0 {
				return nil
			}
		}
	}
	return flush()
}
