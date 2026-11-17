package transferstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const localScanCursorKind = "local-scan-v1"

const (
	// localScanWindow is the most entry lookups one scan has in flight. An
	// entry lookup is one metadata round trip on the source: on NFS a
	// LOOKUP or GETATTR, often tens of milliseconds. 32 of them in flight
	// make 1,600 lookups a second at 20 ms; strand at most 32 threads on a
	// hung mount; use half of Linux's default 64 NFSv4.1 session slots,
	// leaving the rest to transfers; and bound one scan, so a hung source
	// cannot stall another task's scan. It is not derived from the local
	// operation limit: that is a rate without burst which every lookup waits
	// in, so lookups in flight settle at min(32, rate x round trip) anyway.
	// It bounds the lookups in flight only: results that finish ahead of a
	// slower one wait in their directory's ring (see fill), and the slots
	// they free go to the next names, so one slow lookup delays its own
	// entry and not the 31 behind it.
	localScanWindow = 32
	// localScanNames is how many names one directory read asks for: about
	// one 64 KiB NFS READDIR reply, and one step of reported progress.
	localScanNames = 1024
	// localScanEarlyFlush is the listing length after which the open page
	// is saved, if it holds objects, before the listing completes. Objects
	// already found then never wait for a large child directory.
	localScanEarlyFlush = 4096
)

// Names and directory identities are sufficient to reopen only the active DFS
// path. Completed subtrees are never entered again. A very large active directory
// must still have its names enumerated and sorted on recovery, but previously
// consumed files are not stat'ed and no full directory inventory is persisted.
type localScanFrame struct {
	Name     string `json:"name,omitempty"`
	Identity string `json:"identity"`
	After    string `json:"after,omitempty"`
	// Streamed marks a directory that a scan examined in readdir order, as
	// an Agent that streams long directories does: files after After may be
	// found already, so the cursor resumes correctly only through
	// ListOptions.Done. The mark stays while the directory is on the path,
	// across resumes and takeovers, whichever order the directory is read in
	// then; scans that predate it ignore it.
	Streamed bool `json:"streamed,omitempty"`

	key string
	// dir is this directory: the store's root for frame 0 and a root of its
	// own below it, so an entry lookup is one fstatat on this directory's
	// descriptor instead of a walk from the top. os.Root still confines it:
	// no lookup can follow a link out of the directory.
	dir *os.Root
	// stat is dir as opened. Its link, the frame's name in its parent, must
	// still lead here whenever objects found through dir leave the scan.
	stat    fs.FileInfo
	listing *os.File // dir opened to read its names, until they are read
	loading bool     // its names are being read
	read    bool
	names   []string // sorted and distinct, once read
	next    int      // the first name not yet consumed
	// Names in [next, issue) have been considered for a lookup; ring holds
	// the lookups issued among them, in name order.
	issue int
	ring  []*localLookup
	done  localDoneAnswers
	found bool // the open page holds objects found below this directory

	// A directory whose listing passes streamAfter names is streamed: it is
	// examined in readdir order while stream reads the rest, and marked as
	// Streamed. Its After stays where it was, as its files are not done in
	// name order: a resumed scan reads the directory again and finds them
	// done. Its subdirectories are deferred, then entered in sorted order once
	// the listing has ended, each with the identity its lookup found.
	stream       *nameStream
	listed       int // names its listing returned
	deferred     []deferredDirectory
	deferredDirs map[string]fs.FileInfo // after the listing: names holds these
	pageKeys     map[string]struct{}    // files emitted in the open page
}

// name is the frame's name at index: from its sorted names, or from the
// stream in readdir order.
func (frame *localScanFrame) name(index int) string {
	if frame.stream != nil {
		return frame.stream.at(index)
	}
	return frame.names[index]
}

// length is how many names the frame can consume now.
func (frame *localScanFrame) length() int {
	if frame.stream != nil {
		available, _, _ := frame.stream.available()
		return available
	}
	return len(frame.names)
}

// progress is the frame's name count and next index as the activity shows
// them. After a streamed listing, the deferred subdirectories are its last
// entries.
func (frame *localScanFrame) progress() (names, next int) {
	if frame.deferredDirs != nil {
		return frame.listed, frame.listed - (len(frame.names) - frame.next)
	}
	return len(frame.names), frame.next
}

// localDoneAnswers keeps ListOptions.Done's answers for a frame's files.
type localDoneAnswers struct {
	through int              // files before this index have been asked about
	done    map[int]struct{} // indexes of files reported done
	err     error            // the question for the files from errAt failed
	errAt   int
}

// localLookup is one entry examined by a worker. Its fields are written
// before done is closed and read only after that.
type localLookup struct {
	index   int
	done    chan struct{}
	phase   atomic.Int32
	info    fs.FileInfo
	err     error
	object  Object
	keep    bool // an in-scope file that is not a shared-storage probe
	staging bool // a directory that is one of this tool's staging namespaces
}

// localScanFS is the scan's access to the source. Tests substitute it to
// count, gate or fail single calls.
type localScanFS interface {
	Lstat(dir *os.Root, name string) (fs.FileInfo, error)
	OpenRoot(dir *os.Root, name string) (*os.Root, error)
	OpenDir(dir *os.Root) (*os.File, error)
	ReadNames(file *os.File, n int) ([]string, error)
}

type rootScanFS struct{}

func (rootScanFS) Lstat(dir *os.Root, name string) (fs.FileInfo, error) { return dir.Lstat(name) }
func (rootScanFS) OpenRoot(dir *os.Root, name string) (*os.Root, error) { return dir.OpenRoot(name) }
func (rootScanFS) OpenDir(dir *os.Root) (*os.File, error)               { return dir.Open(".") }

// ReadNames reads names only, like ls -f. File.ReadDir would look up every
// entry of a directory opened through an os.Root before returning any.
func (rootScanFS) ReadNames(file *os.File, n int) ([]string, error) { return file.Readdirnames(n) }

// localScanTuning replaces the scan's source access and bounds in tests.
type localScanTuning struct {
	fs           localScanFS
	window       int
	earlyFlush   int
	streamAfter  int
	streamBuffer int
	holdLookups  bool // lookups wait for the end of a streamed listing or a full buffer
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
//
// A directory's names are read first, without looking any entry up, then
// sorted. Each entry is then looked up once, relative to its directory's own
// handle, with up to localScanWindow lookups in flight; entries are still
// consumed one by one in name order, so pages, cursors and errors are those
// of a scan that looks entries up one at a time.
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
	saved, err := s.scanFrames(options)
	if err != nil {
		return err
	}
	scan := s.newScan(ctx, options, limit, emit)
	defer scan.close()
	options.Activity.start(ScanModeSorted)
	// Validate every directory on a resumed path before yielding any objects.
	if err := scan.resume(saved); err != nil {
		return err
	}
	return scan.run()
}

type localScan struct {
	s                   *Local
	ctx                 context.Context
	options             ListOptions
	limit               int
	emit                func(Page) error
	activity            *ScanActivity
	fs                  localScanFS
	window              int
	earlyFlush          int
	streamAfter         int
	streamBuffer        int
	lookupsWhileListing bool
	frames              []*localScanFrame
	objects             []Object
	visited             int
	slots               chan struct{} // one per lookup in flight
	released            chan struct{} // signalled whenever a lookup returns its slot
	workers             sync.WaitGroup
	// page is cancelled when the open page ends, releasing the lookups that
	// still wait for the local operation limit.
	page    context.Context
	endPage context.CancelFunc
}

func (s *Local) newScan(ctx context.Context, options ListOptions, limit int, emit func(Page) error) *localScan {
	scan := &localScan{s: s, ctx: ctx, options: options, limit: limit, emit: emit, activity: options.Activity,
		fs: rootScanFS{}, window: localScanWindow, earlyFlush: localScanEarlyFlush, streamAfter: localScanStreamAfter,
		streamBuffer: localScanStreamBuffer, lookupsWhileListing: localScanLookupsWhileListing, objects: make([]Object, 0, limit)}
	if tuning := s.scanTuning; tuning != nil {
		if tuning.fs != nil {
			scan.fs = tuning.fs
		}
		if tuning.window > 0 {
			scan.window = tuning.window
		}
		if tuning.earlyFlush > 0 {
			scan.earlyFlush = tuning.earlyFlush
		}
		if tuning.streamAfter > 0 {
			scan.streamAfter = tuning.streamAfter
		}
		if tuning.streamBuffer > 0 {
			scan.streamBuffer = tuning.streamBuffer
		}
		scan.lookupsWhileListing = scan.lookupsWhileListing && !tuning.holdLookups
	}
	scan.slots, scan.released = make(chan struct{}, scan.window), make(chan struct{}, 1)
	scan.page, scan.endPage = context.WithCancel(ctx)
	return scan
}

func (scan *localScan) top() *localScanFrame { return scan.frames[len(scan.frames)-1] }

func (scan *localScan) run() error {
	for len(scan.frames) > 0 {
		if err := scan.ctx.Err(); err != nil {
			return err
		}
		frame := scan.top()
		if !frame.read && frame.stream == nil {
			if err := scan.load(frame, false); err != nil {
				return err
			}
		}
		if frame.stream != nil {
			more, err := scan.awaitName(frame)
			if err != nil {
				return err
			}
			if !more {
				scan.finishStream(frame)
				continue
			}
		} else if frame.next == len(frame.names) {
			if err := scan.pop(); err != nil {
				return err
			}
			continue
		}
		if err := scan.step(frame); err != nil {
			return err
		}
		if scan.visited >= scan.limit {
			// A page without objects while a frame streams would save the
			// cursor it saved before: a streamed frame's After does not move.
			if frame.stream != nil && len(scan.objects) == 0 {
				scan.visited = 0
				continue
			}
			if err := scan.flush(); err != nil {
				return err
			}
			if len(scan.frames) == 0 {
				return nil
			}
		}
	}
	return scan.flush()
}

// resume reopens a saved path from the root down. Each directory must still be
// linked where it was and be the same directory. Only the deepest one is read
// now; the others are read when the scan returns to them.
func (scan *localScan) resume(saved []localScanFrame) error {
	for index := range saved {
		if err := scan.ctx.Err(); err != nil {
			return err
		}
		frame := &localScanFrame{Name: saved[index].Name, Identity: saved[index].Identity, After: saved[index].After,
			Streamed: saved[index].Streamed, key: saved[index].key}
		var err error
		if index == 0 {
			err = scan.openRootFrame(frame)
		} else {
			err = scan.reopenFrame(scan.top(), frame)
		}
		if err != nil {
			return err
		}
		scan.frames = append(scan.frames, frame)
		if index < len(saved)-1 {
			// One listing is open at a time: the frame being read.
			_ = frame.listing.Close()
			frame.listing = nil
		}
	}
	return nil
}

func (scan *localScan) openRootFrame(frame *localScanFrame) error {
	file, info, err := scan.openDir(scan.s.root, frame.key)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		_ = file.Close()
		return ErrSourceChanged
	}
	if err := frame.adopt(file, info); err != nil {
		_ = file.Close()
		return err
	}
	frame.dir = scan.s.root
	return nil
}

// reopenFrame opens a saved directory below parent: its link must name a
// directory, which must be the one the cursor saved.
func (scan *localScan) reopenFrame(parent, frame *localScanFrame) error {
	linked, err := scan.lstat(parent.dir, frame.Name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: scan directory %q is missing", ErrSourceChanged, frame.key)
		}
		return scanPathError(err, frame.key)
	}
	if !linked.IsDir() {
		return fmt.Errorf("%w: scan directory %q was replaced", ErrSourceChanged, frame.key)
	}
	dir, err := scan.openRoot(parent.dir, frame.Name)
	if err != nil {
		return scanPathError(err, frame.key)
	}
	file, info, err := scan.openDir(dir, frame.key)
	if err == nil && (!info.IsDir() || !os.SameFile(linked, info)) {
		_ = file.Close()
		err = ErrSourceChanged
	}
	if err == nil {
		if err = frame.adopt(file, info); err != nil {
			_ = file.Close()
		}
	}
	if err != nil {
		_ = dir.Close()
		return err
	}
	frame.dir = dir
	return nil
}

// adopt checks an opened directory against the identity a cursor saved for
// the frame, and keeps it open for reading the frame's names.
func (frame *localScanFrame) adopt(file *os.File, info fs.FileInfo) error {
	identity, err := openedIdentity(file, info)
	if err != nil {
		return err
	}
	if frame.Identity != "" && frame.Identity != identity {
		return fmt.Errorf("%w: scan directory %q was replaced", ErrSourceChanged, frame.key)
	}
	frame.Identity, frame.stat, frame.listing = identity, info, file
	return nil
}

// load reads the frame's names, like ls -f: names only, with no lookup of any
// entry, and then sorts them, so the frame continues after its saved After
// and saves After again. When early is set and the listing passes
// earlyFlush names, a page holding objects is saved before reading on: the
// objects found so far need not wait for the rest of a large listing. A
// listing that passes streamAfter names streams instead (see startStream).
func (scan *localScan) load(frame *localScanFrame, early bool) error {
	file := frame.listing
	frame.listing = nil
	if file == nil {
		var info fs.FileInfo
		var err error
		if file, info, err = scan.openDir(frame.dir, frame.key); err != nil {
			return err
		}
		if !info.IsDir() || !os.SameFile(frame.stat, info) {
			_ = file.Close()
			return fmt.Errorf("%w: scan directory %q was replaced", ErrSourceChanged, frame.key)
		}
	}
	names, streaming, err := scan.readNames(frame, file, early)
	if streaming {
		scan.startStream(frame, file, names)
		return nil
	}
	_ = file.Close()
	if err != nil {
		return err
	}
	slices.Sort(names)
	frame.names, frame.read = slices.Compact(names), true
	frame.next = sort.Search(len(frame.names), func(index int) bool { return frame.names[index] > frame.After })
	frame.issue = frame.next
	scan.activity.listed(len(frame.names), frame.next)
	return nil
}

// readNames reads a directory listing to its end, or until it passes
// streamAfter names, which it reports as streaming.
func (scan *localScan) readNames(frame *localScanFrame, file *os.File, early bool) ([]string, bool, error) {
	frame.loading = true
	defer func() { frame.loading = false }()
	scan.activity.enter(frame.key, true, 0, 0)
	var names []string
	flushed := !early
	// Each read is one local operation, charged before it is made, except
	// the read after a short one: a read returns fewer names than it asks
	// for only at the end of the listing, and the read after it just
	// confirms that end. Should it return names after all, it is charged.
	short := false
	for {
		if !short {
			if err := scan.operation(); err != nil {
				return nil, false, err
			}
		}
		end := scan.activity.call()
		batch, err := scan.fs.ReadNames(file, localScanNames)
		end()
		if short && len(batch) > 0 {
			if err := scan.operation(); err != nil {
				return nil, false, err
			}
		}
		short = len(batch) < localScanNames
		names = append(names, batch...)
		scan.activity.read(len(batch))
		if err == io.EOF {
			return names, false, nil
		}
		if err != nil {
			return nil, false, scanPathError(err, frame.key)
		}
		if !flushed && len(names) > scan.earlyFlush && len(scan.objects) > 0 {
			flushed = true
			if err := scan.flush(); err != nil {
				return nil, false, err
			}
		}
		if len(names) > scan.streamAfter {
			return names, true, nil
		}
	}
}

// step consumes the frame's next name.
func (scan *localScan) step(frame *localScanFrame) error {
	name := frame.name(frame.next)
	if !frame.pending(name) {
		// Done before the saved position, as in a sorted directory.
		frame.next++
		frame.stream.release(frame.next)
		return nil
	}
	key := path.Join(frame.key, name)
	if info, deferred := frame.deferredDirs[name]; deferred {
		// Examined while its parent streamed.
		scan.consume(frame, entryUnexamined)
		return scan.descend(frame, name, key, info)
	}
	if !scanEntryName(name) {
		return ErrInvalidKey
	}
	if _, emitted := frame.pageKeys[name]; emitted {
		// The listing repeated a file the open page holds already.
		scan.consume(frame, entryAlreadyFound)
		return nil
	}
	file, directory := scan.candidate(key)
	if !file && !directory {
		scan.consume(frame, entryUnexamined)
		return nil
	}
	if file {
		done, err := scan.done(frame, frame.next)
		if err != nil {
			return err
		}
		if done {
			scan.consume(frame, entryAlreadyFound)
			return nil
		}
	}
	lookup, err := scan.head(frame)
	if err != nil {
		return err
	}
	switch info := lookup.info; {
	case errors.Is(lookup.err, fs.ErrNotExist):
		// Gone since the directory was read: as if it had never been listed.
		scan.consume(frame, entryVanished)
	case lookup.err != nil:
		return lookup.err
	case file && info.Mode().IsRegular():
		if lookup.keep {
			scan.objects = append(scan.objects, lookup.object)
			frame.found = true
			if frame.pageKeys != nil {
				frame.pageKeys[name] = struct{}{}
			}
		}
		scan.consume(frame, entryExamined)
	case directory && info.IsDir() && !lookup.staging && frame.stream != nil:
		frame.deferred = append(frame.deferred, deferredDirectory{name: name, info: info})
		scan.consume(frame, entryExamined)
	case directory && info.IsDir() && !lookup.staging:
		// Advance only once the child is open; on failure the caller keeps
		// the preceding durable cursor anyway.
		scan.consume(frame, entryExamined)
		return scan.descend(frame, name, key, info)
	default:
		// Links, special files, directories off the prefix path, staging
		// namespaces and files out of scope are not migration objects.
		scan.consume(frame, entryExamined)
	}
	return nil
}

// candidate reports what an entry can be, by its key alone: a file in the
// listing's scope, or a directory on the way to its prefix.
func (scan *localScan) candidate(key string) (file, directory bool) {
	prefix, startAfter := scan.options.Prefix, scan.options.StartAfter
	file = strings.HasPrefix(key, prefix) && (startAfter == "" || key > startAfter)
	directory = strings.HasPrefix(key+"/", prefix) || strings.HasPrefix(prefix, key+"/")
	return file, directory
}

// consume marks the frame's next name done. A streamed frame keeps its After:
// names after it are done in readdir order, not in name order.
func (scan *localScan) consume(frame *localScanFrame, outcome int) {
	if len(frame.ring) > 0 && frame.ring[0].index == frame.next {
		// Issued, then not needed: a name the open page holds already. Its
		// worker finishes on its own.
		frame.ring[0] = nil
		frame.ring = frame.ring[1:]
	}
	if frame.stream != nil {
		frame.next++
		frame.stream.release(frame.next)
	} else {
		frame.After, frame.next = frame.names[frame.next], frame.next+1
	}
	scan.visited++
	_, next := frame.progress()
	scan.activity.consumed(outcome, next)
}

// pending reports whether a name of the frame is still to be done: a
// streamed frame has done the names up to its After already.
func (frame *localScanFrame) pending(name string) bool {
	return frame.stream == nil || name > frame.After
}

// done reports whether ListOptions.Done has the file at index as committed in
// this pass. It is asked about up to localScanNames files at a time, from the
// first unanswered one on; a failure is reported at that file.
func (scan *localScan) done(frame *localScanFrame, index int) (bool, error) {
	if scan.options.Done == nil {
		return false, nil
	}
	answers := &frame.done
	if answers.err != nil && index >= answers.errAt {
		return false, answers.err
	}
	if index >= answers.through {
		if err := scan.askDone(frame, index); err != nil {
			answers.err, answers.errAt = err, index
			return false, err
		}
	}
	_, done := answers.done[index]
	return done, nil
}

func (scan *localScan) askDone(frame *localScanFrame, from int) error {
	var keys []string
	var indexes []int
	index, end := from, frame.length()
	for ; index < end && len(keys) < localScanNames; index++ {
		name := frame.name(index)
		if !frame.pending(name) || !scanEntryName(name) {
			continue
		}
		key := path.Join(frame.key, name)
		if file, _ := scan.candidate(key); file {
			keys, indexes = append(keys, key), append(indexes, index)
		}
	}
	answers, err := scan.options.Done(keys)
	if err == nil && len(answers) != len(keys) {
		err = fmt.Errorf("scan progress check answered %d of %d keys", len(answers), len(keys))
	}
	if err != nil {
		return err
	}
	// Answers behind the head are no longer needed.
	for position := range frame.done.done {
		if position < frame.next {
			delete(frame.done.done, position)
		}
	}
	if frame.done.done == nil {
		frame.done.done = map[int]struct{}{}
	}
	for i, done := range answers {
		if done {
			frame.done.done[indexes[i]] = struct{}{}
		}
	}
	frame.done.through = index
	return nil
}

// head returns the finished lookup of the frame's next name.
func (scan *localScan) head(frame *localScanFrame) (*localLookup, error) {
	scan.fill(frame, false)
	if len(frame.ring) == 0 || frame.ring[0].index != frame.next {
		// fill issues the head unless the scan stopped while it waited for
		// a free slot.
		if err := scan.ctx.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("scan lookup was not issued")
	}
	lookup := frame.ring[0]
	if err := scan.await(frame, lookup); err != nil {
		return nil, err
	}
	frame.ring[0] = nil
	frame.ring = frame.ring[1:]
	return lookup, nil
}

// fill issues lookups for the frame's names from its head on, in order. It
// stops at the last name the open page can reach, so no lookup outlives its
// page in a flat directory, and at window lookups in the frame's ring, in
// flight or parked. Behind a head that is still being looked up, the ring
// grows on up to parked: the window then bounds only the lookups in flight,
// and the slots of those that finish go to the next names. The head's own
// lookup waits for a free slot; the others are issued only while one is free.
func (scan *localScan) fill(frame *localScanFrame, behind bool) {
	if frame.deferredDirs != nil {
		return // Deferred directories were looked up as their parent streamed.
	}
	// Names consumed without a lookup leave issue behind; a stream has
	// released them.
	frame.issue = max(frame.issue, frame.next)
	end := min(frame.length(), frame.next+scan.limit-scan.visited)
	held := scan.window
	if behind {
		held = scan.parked(frame)
	}
	for frame.issue < end && len(frame.ring) < held {
		index := frame.issue
		name := frame.name(index)
		if !frame.pending(name) {
			frame.issue++
			continue
		}
		if !scanEntryName(name) {
			return // The scan fails there; nothing after it is needed.
		}
		key := path.Join(frame.key, name)
		file, directory := scan.candidate(key)
		if !file && !directory {
			frame.issue++
			continue
		}
		if file {
			done, err := scan.done(frame, index)
			if err != nil {
				return
			}
			if done {
				frame.issue++
				continue
			}
		}
		if !scan.acquire(index == frame.next) {
			return
		}
		frame.ring = append(frame.ring, scan.lookup(frame, index, name, key, file, directory))
		frame.issue++
	}
}

// parked is how many lookups, finished or not, frame may hold in its ring
// behind a head that is still being looked up: a page's worth, less what its
// ancestors still hold from before the scan entered it, and never less than
// the window. Results a directory holds as the scan enters a subdirectory
// wait for the scan's return; if the page ends first, they are dropped like
// any other result the page did not reach. A chain of directories, each
// entered while it held a page's worth, holds at most a page and a window
// per directory.
func (scan *localScan) parked(frame *localScanFrame) int {
	ancestors := 0
	for _, other := range scan.frames {
		if other != frame {
			ancestors += len(other.ring)
		}
	}
	return max(scan.window, scan.limit-ancestors)
}

// notify signals a channel of capacity one without waiting: a signal sent
// while another is pending merges with it.
func notify(signal chan struct{}) {
	select {
	case signal <- struct{}{}:
	default:
	}
}

func (scan *localScan) acquire(wait bool) bool {
	select {
	case scan.slots <- struct{}{}:
		return true
	default:
	}
	if !wait {
		return false
	}
	select {
	case scan.slots <- struct{}{}:
		return true
	case <-scan.ctx.Done():
		return false
	}
}

// await waits for the lookup at the head of frame. Each lookup that returns
// its slot meanwhile lets fill issue the next name, so the lookups behind a
// slow head keep the window busy instead of finishing one by one and leaving
// it empty.
func (scan *localScan) await(frame *localScanFrame, lookup *localLookup) error {
	select {
	case <-lookup.done:
		return nil
	default:
	}
	defer scan.activity.await(&lookup.phase)()
	for {
		select {
		case <-lookup.done:
			return nil
		case <-scan.released:
			scan.fill(frame, true)
		case <-scan.ctx.Done():
			return scan.ctx.Err()
		}
	}
}

func (scan *localScan) lookup(frame *localScanFrame, index int, name, key string, file, directory bool) *localLookup {
	lookup := &localLookup{index: index, done: make(chan struct{})}
	dir, ctx := frame.dir, scan.page
	scan.workers.Add(1)
	go func() {
		defer scan.workers.Done()
		defer func() {
			<-scan.slots
			notify(scan.released)
		}()
		defer close(lookup.done)
		scan.examine(ctx, lookup, dir, name, key, file, directory)
		lookup.phase.Store(lookupFinished)
	}()
	return lookup
}

// examine is one worker's lookup: one fstatat on the parent's descriptor,
// then for a file in scope the probe check and its object, and for a
// directory whether it is a staging namespace. Errors name the entry's key.
func (scan *localScan) examine(ctx context.Context, lookup *localLookup, dir *os.Root, name, key string, file, directory bool) {
	if lookup.err = scan.wait(ctx, lookup); lookup.err != nil {
		return
	}
	end := scan.activity.call()
	started := time.Now()
	info, err := scan.fs.Lstat(dir, name)
	scan.activity.lookup(time.Since(started))
	end()
	if err != nil {
		lookup.err = scanPathError(err, key)
		return
	}
	lookup.info = info
	switch {
	case file && info.Mode().IsRegular():
		probe, err := scan.probe(ctx, lookup, dir, name, key, info)
		if err != nil || probe {
			lookup.err = err
			return
		}
		lookup.object, lookup.err = scan.object(ctx, lookup, dir, name, key, info)
		lookup.keep = lookup.err == nil
	case directory && info.IsDir():
		lookup.staging, lookup.err = scan.staging(ctx, lookup, dir, name)
	}
}

// wait charges one local operation to a worker's lookup. The scan shows it
// waiting for the local limit while it waits for this lookup.
func (scan *localScan) wait(ctx context.Context, lookup *localLookup) error {
	if scan.s.localIO == nil {
		return ctx.Err()
	}
	lookup.phase.Store(lookupWaitingLimit)
	defer lookup.phase.Store(lookupPending)
	return scan.s.localIO.WaitLocalOperation(ctx)
}

// probe reports whether a file is a shared-storage probe, which is not a
// migration object. Only a file with a probe's name and size is opened: one
// local operation, its bytes charged to local bandwidth.
func (scan *localScan) probe(ctx context.Context, lookup *localLookup, dir *os.Root, name, key string, info fs.FileInfo) (bool, error) {
	size, candidate := sharedProbeCandidate(name, info)
	if !candidate {
		return false, nil
	}
	if err := scan.wait(ctx, lookup); err != nil {
		return false, err
	}
	if err := scan.s.localBytes(ctx, size); err != nil {
		return false, err
	}
	end := scan.activity.call()
	defer end()
	probe, err := isSharedProbeFile(ctx, dir, name, info)
	return probe, scanPathError(err, key)
}

// sharedProbeCandidate reports whether isSharedProbeFile opens a file, and
// how many bytes it then reads.
func sharedProbeCandidate(name string, info fs.FileInfo) (int, bool) {
	if !strings.HasPrefix(name, sharedProbePrefix) || !info.Mode().IsRegular() {
		return 0, false
	}
	template, err := probeContents(SharedProbe{Key: name, Nonce: strings.Repeat("0", 64)})
	if err != nil || info.Size() != int64(len(template)) {
		return 0, false
	}
	return len(template), true
}

// object is a scanned file's Object, as storageObject builds it from the same
// lookup. Where the identity needs an open handle, that open is one more
// local operation.
func (scan *localScan) object(ctx context.Context, lookup *localLookup, dir *os.Root, name, key string, info fs.FileInfo) (Object, error) {
	object := localObject(key, info)
	if !localIdentityOpens {
		return object, nil
	}
	if err := scan.wait(ctx, lookup); err != nil {
		return Object{}, err
	}
	end := scan.activity.call()
	identity, err := storageIdentity(dir, name, info)
	end()
	if err != nil {
		return Object{}, scanPathError(err, key)
	}
	object.ETag = identityFingerprint(info, identity)
	return object, nil
}

// staging reports whether a directory is one of this tool's staging
// namespaces, which a scan never enters. Only a directory with such a name is
// opened and its marker read: two local operations.
func (scan *localScan) staging(ctx context.Context, lookup *localLookup, dir *os.Root, name string) (bool, error) {
	owner, id, ok := localTempNamespaceName(name)
	if !ok {
		return false, nil
	}
	if err := scan.wait(ctx, lookup); err != nil {
		return false, err
	}
	end := scan.activity.call()
	directory, err := scan.fs.OpenRoot(dir, name)
	end()
	if err != nil {
		return false, nil // As isLocalTempNamespace: an unopenable directory is no namespace.
	}
	defer directory.Close()
	if err := scan.wait(ctx, lookup); err != nil {
		return false, err
	}
	if err := scan.s.localBytes(ctx, len(namespaceContents(owner, id))); err != nil {
		return false, err
	}
	end = scan.activity.call()
	defer end()
	return validNamespaceMarker(directory, owner, id), nil
}

// descend opens the directory an entry lookup found and makes it the top
// frame. If it cannot be opened as that directory, childRule decides.
func (scan *localScan) descend(parent *localScanFrame, name, key string, looked fs.FileInfo) error {
	dir, openErr := scan.openRoot(parent.dir, name)
	var file *os.File
	var info fs.FileInfo
	if openErr == nil {
		if file, info, openErr = scan.openDir(dir, key); openErr != nil {
			_ = dir.Close()
			dir = nil
		}
	}
	if openErr != nil || !info.IsDir() || !os.SameFile(looked, info) {
		proceed, err := scan.childRule(parent, name, key, looked, info, openErr)
		if !proceed {
			if file != nil {
				_ = file.Close()
				_ = dir.Close()
			}
			return err
		}
	}
	child := &localScanFrame{Name: name, key: key, dir: dir}
	if err := child.adopt(file, info); err != nil {
		_ = file.Close()
		_ = dir.Close()
		return err
	}
	scan.frames = append(scan.frames, child)
	return scan.load(child, true)
}

// childRule decides an entry whose directory could not be opened as the one
// its lookup found, from one fresh lookup:
//   - nothing there: the directory vanished and the scan goes on without it;
//   - a directory, the one opened: it was replaced before the open, and the
//     scan enters the replacement;
//   - the directory the lookup found: the open's own error, as before;
//   - anything else: the source changed.
func (scan *localScan) childRule(parent *localScanFrame, name, key string, looked, opened fs.FileInfo, openErr error) (bool, error) {
	current, err := scan.lstat(parent.dir, name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, scanPathError(err, key)
	case !current.IsDir():
	case openErr == nil && opened.IsDir() && os.SameFile(current, opened):
		return true, nil
	case openErr != nil && os.SameFile(current, looked):
		return false, scanPathError(openErr, key)
	}
	return false, fmt.Errorf("%w: scan directory %q was replaced", ErrSourceChanged, key)
}

// pop leaves the top directory, which is done. When the open page holds
// objects found below it, its link is checked now, as it will not be on the
// stack when the page ends; its parent then holds such objects too.
func (scan *localScan) pop() error {
	frame := scan.top()
	if frame.found && len(scan.frames) > 1 {
		parent := scan.frames[len(scan.frames)-2]
		if err := scan.checkLink(parent, frame); err != nil {
			return err
		}
		parent.found = true
	}
	scan.frames = scan.frames[:len(scan.frames)-1]
	scan.closeFrame(frame)
	if len(scan.frames) > 0 {
		if top := scan.top(); top.read {
			names, next := top.progress()
			scan.activity.enter(top.key, false, names, next)
		}
	}
	return nil
}

func (scan *localScan) closeFrame(frame *localScanFrame) {
	if frame.stream != nil {
		frame.stream.close()
		frame.stream = nil
	}
	if frame.listing != nil {
		_ = frame.listing.Close()
		frame.listing = nil
	}
	if frame.dir != scan.s.root {
		_ = frame.dir.Close()
	}
}

// streamed reports whether a directory on the cursor's path is marked as
// streamed: the cursor then resumes correctly only through ListOptions.Done
// (see Page.Streamed).
func (scan *localScan) streamed() bool {
	for _, frame := range scan.frames {
		if frame.Streamed {
			return true
		}
	}
	return false
}

// flush ends the open page. No lookup outlives it, and every directory the
// cursor names must still be linked where it was before the page is handed
// on. The links are checked again after the callback, which may have taken
// long, before any further lookup goes through them.
func (scan *localScan) flush() error {
	scan.settle()
	if err := scan.popExhausted(); err != nil {
		return err
	}
	if err := scan.checkLinks(); err != nil {
		return err
	}
	if err := scan.ctx.Err(); err != nil {
		return err
	}
	cursor, err := scan.cursor()
	if err != nil {
		return err
	}
	if err := scan.emit(Page{Objects: scan.objects, NextCursor: cursor, Streamed: scan.streamed()}); err != nil {
		return err
	}
	scan.activity.Saved()
	scan.objects, scan.visited = make([]Object, 0, scan.limit), 0
	for _, frame := range scan.frames {
		frame.found = false
		if frame.pageKeys != nil {
			frame.pageKeys = map[string]struct{}{}
		}
		if frame.stream != nil {
			// A name the listing repeats may be one of the files just handed
			// on, which pageKeys no longer holds. Done was asked about the
			// repeat before then and said no: ask again from the head on.
			frame.done = localDoneAnswers{through: frame.next, err: frame.done.err, errAt: frame.done.errAt}
		}
	}
	if err := scan.ctx.Err(); err != nil {
		return err
	}
	return scan.checkLinks()
}

// settle ends the page's lookups: those still waiting for the local limit
// are cancelled, those in a source call are awaited, and every result not
// consumed is dropped, to be looked up again when the scan reaches it.
func (scan *localScan) settle() {
	scan.endPage()
	scan.workers.Wait()
	for _, frame := range scan.frames {
		clear(frame.ring)
		frame.ring, frame.issue = frame.ring[:0], frame.next
	}
	scan.page, scan.endPage = context.WithCancel(scan.ctx)
}

// popExhausted leaves the directories that are done, as the cursor must not
// name them. A directory the scan returns to is read first to know.
func (scan *localScan) popExhausted() error {
	for len(scan.frames) > 0 {
		frame := scan.top()
		if frame.loading {
			return nil
		}
		if !frame.read && frame.stream == nil {
			if err := scan.load(frame, false); err != nil {
				return err
			}
		}
		if frame.stream != nil || frame.next < len(frame.names) {
			return nil // A streamed listing is not done until it ends.
		}
		if err := scan.pop(); err != nil {
			return err
		}
	}
	return nil
}

// checkLinks checks every stacked directory's link at once, each through its
// parent's handle: one round trip however deep the stack, not one walk from
// the top per directory.
func (scan *localScan) checkLinks() error {
	if len(scan.frames) < 2 {
		return nil
	}
	errs := make([]error, len(scan.frames))
	slots := make(chan struct{}, scan.window)
	var checks sync.WaitGroup
	for index := 1; index < len(scan.frames); index++ {
		slots <- struct{}{}
		checks.Add(1)
		go func() {
			defer checks.Done()
			defer func() { <-slots }()
			errs[index] = scan.checkLink(scan.frames[index-1], scan.frames[index])
		}()
	}
	checks.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// checkLink verifies that frame's name in its parent still leads to the
// directory the frame has open. A directory renamed away, or replaced by
// another directory or a link, would otherwise let objects found through its
// handle leave the scan under a path that no longer leads to them.
func (scan *localScan) checkLink(parent, frame *localScanFrame) error {
	linked, err := scan.lstat(parent.dir, frame.Name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%w: scan directory %q is missing", ErrSourceChanged, frame.key)
	case err != nil:
		return scanPathError(err, frame.key)
	case !linked.IsDir() || !os.SameFile(linked, frame.stat):
		return fmt.Errorf("%w: scan directory %q was replaced", ErrSourceChanged, frame.key)
	}
	return nil
}

func (scan *localScan) cursor() (string, error) {
	frames := make([]localScanFrame, len(scan.frames))
	for index, frame := range scan.frames {
		frames[index] = localScanFrame{Name: frame.Name, Identity: frame.Identity, After: frame.After, Streamed: frame.Streamed}
	}
	return scan.s.scanCursor(scan.options, frames)
}

// close stops the workers before the handles they use are closed.
func (scan *localScan) close() {
	scan.endPage()
	scan.workers.Wait()
	for _, frame := range scan.frames {
		scan.closeFrame(frame)
	}
}

// operation charges one local operation to the scan itself, which waits for
// the local limit meanwhile.
func (scan *localScan) operation() error {
	if scan.s.localIO == nil {
		return scan.ctx.Err()
	}
	defer scan.activity.limit()()
	return scan.s.localIO.WaitLocalOperation(scan.ctx)
}

// lstat is one lookup by the scan itself: a link check or the child rule.
func (scan *localScan) lstat(dir *os.Root, name string) (fs.FileInfo, error) {
	if err := scan.operation(); err != nil {
		return nil, err
	}
	defer scan.activity.call()()
	return scan.fs.Lstat(dir, name)
}

func (scan *localScan) openRoot(dir *os.Root, name string) (*os.Root, error) {
	if err := scan.operation(); err != nil {
		return nil, err
	}
	defer scan.activity.call()()
	return scan.fs.OpenRoot(dir, name)
}

// openDir opens a directory to read its names: one local operation for the
// open and its fstat.
func (scan *localScan) openDir(dir *os.Root, key string) (*os.File, fs.FileInfo, error) {
	if err := scan.operation(); err != nil {
		return nil, nil, err
	}
	defer scan.activity.call()()
	file, err := scan.fs.OpenDir(dir)
	if err != nil {
		return nil, nil, scanPathError(err, key)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, scanPathError(err, key)
	}
	return file, info, nil
}

// scanPathError names an entry by its key in a path error, rather than by the
// name its directory's root resolved.
func scanPathError(err error, key string) error {
	if pathError, ok := err.(*fs.PathError); ok && pathError.Path != key {
		copied := *pathError
		copied.Path = key
		return &copied
	}
	return err
}
