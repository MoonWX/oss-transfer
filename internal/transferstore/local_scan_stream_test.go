package transferstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// streamingTree is a random tree with directories long enough to stream at
// streamAfter 3, some of them nested in others.
func streamingTree(t *testing.T, seed int64) string {
	t.Helper()
	root := randomScanTree(t, seed, false)
	for _, directory := range []string{"big", "big/inner", "deep/long"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(directory)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 9 + int(seed%5) {
		writeFiles(t, root, fmt.Sprintf("big/f%02d", i), fmt.Sprintf("big/inner/g%02d", i), fmt.Sprintf("deep/long/h%02d", i))
	}
	writeFiles(t, root, "big/sub/x", "big/zz/y")
	return root
}

// committedKeys plays discovery: keys of every page handed on are committed,
// and Done reports them.
type committedKeys struct {
	mu   sync.Mutex
	keys map[string]int
}

func (c *committedKeys) done(keys []string) ([]bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	answers := make([]bool, len(keys))
	for i, key := range keys {
		answers[i] = c.keys[key] > 0
	}
	return answers, nil
}

func (c *committedKeys) commit(page Page) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, object := range page.Objects {
		c.keys[object.Key]++
	}
}

func streamingLocal(t *testing.T, root string, tuning localScanTuning) *Local {
	t.Helper()
	local, err := NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	local.scanTuning = &tuning
	return local
}

func scanObjects(t *testing.T, root string) map[string]Object {
	t.Helper()
	local, err := NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	objects := map[string]Object{}
	if err := local.Scan(context.Background(), ListOptions{}, func(object Object) error { objects[object.Key] = object; return nil }); err != nil {
		t.Fatal(err)
	}
	return objects
}

// Resumed after every page, a scan that streams long directories commits
// exactly the objects Scan streams, each once, whatever the page size and
// the read-ahead buffer.
func TestLocalScanStreamingResumesExactlyOnce(t *testing.T) {
	for seed := range int64(8) {
		root := streamingTree(t, seed)
		want := scanObjects(t, root)
		for _, limit := range []int{1, 2, 3, 5, 7} {
			for _, buffer := range []int{1, 64, 1 << 20} {
				committed := &committedKeys{keys: map[string]int{}}
				options := ListOptions{Limit: limit, Done: committed.done}
				streamedPages := 0
				for pages := 0; ; pages++ {
					if pages > 2000 {
						t.Fatal("scan made no progress")
					}
					local := streamingLocal(t, root, localScanTuning{streamAfter: 3, earlyFlush: 3, streamBuffer: buffer})
					stop := errors.New("page saved")
					var page Page
					err := local.ScanPages(context.Background(), options, func(p Page) error { page = p; return stop })
					_ = local.Close()
					if !errors.Is(err, stop) {
						t.Fatalf("seed %d limit %d buffer %d: %v", seed, limit, buffer, err)
					}
					committed.commit(page)
					if page.Streamed {
						streamedPages++
					}
					if options.Cursor = page.NextCursor; options.Cursor == "" {
						break
					}
				}
				for key, n := range committed.keys {
					if n != 1 {
						t.Fatalf("seed %d limit %d buffer %d: %s committed %d times", seed, limit, buffer, key, n)
					}
				}
				if len(committed.keys) != len(want) {
					t.Fatalf("seed %d limit %d buffer %d: committed %d of %d objects", seed, limit, buffer, len(committed.keys), len(want))
				}
				for key := range want {
					if committed.keys[key] != 1 {
						t.Fatalf("seed %d: %s missing", seed, key)
					}
				}
				if limit < 5 && streamedPages == 0 {
					t.Fatalf("seed %d limit %d: no page named a streamed directory", seed, limit)
				}
			}
		}
	}
}

// A streamed cursor stays a v1 position: a sorted scan resumes it and, with
// the committed keys, commits each remaining object once; the released scan,
// which knows nothing of them, processes a superset that misses nothing.
func TestLocalScanStreamedCursorResumesInOlderScans(t *testing.T) {
	root := streamingTree(t, 3)
	want := scanObjects(t, root)
	committed := &committedKeys{keys: map[string]int{}}
	options := ListOptions{Limit: 2, Done: committed.done}
	local := streamingLocal(t, root, localScanTuning{streamAfter: 3, earlyFlush: 3})
	defer local.Close()
	stop := errors.New("saved")
	for {
		var page Page
		if err := local.ScanPages(context.Background(), options, func(p Page) error { page = p; return stop }); !errors.Is(err, stop) {
			t.Fatal(err)
		}
		committed.commit(page)
		options.Cursor = page.NextCursor
		if page.Streamed && page.NextCursor != "" && len(committed.keys) > 3 {
			break
		}
		if page.NextCursor == "" {
			t.Fatal("no streamed cursor to resume")
		}
	}
	before := map[string]int{}
	for key, n := range committed.keys {
		before[key] = n
	}
	// Stage one's sorted scan, with Done.
	sorted := streamingLocal(t, root, localScanTuning{streamAfter: 1 << 30})
	defer sorted.Close()
	if err := sorted.ScanPages(context.Background(), options, func(p Page) error { committed.commit(p); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(committed.keys) != len(want) {
		t.Fatalf("sorted resume committed %d of %d objects", len(committed.keys), len(want))
	}
	for key, n := range committed.keys {
		if n != 1 {
			t.Fatalf("sorted resume committed %s %d times", key, n)
		}
	}
	// The released scan, without Done.
	seen := map[string]bool{}
	for key := range before {
		seen[key] = true
	}
	options.Done = nil
	if err := sorted.headScanPages(context.Background(), options, func(p Page) error {
		for _, object := range p.Objects {
			seen[object.Key] = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for key := range want {
		if !seen[key] {
			t.Fatalf("released scan missed %s", key)
		}
	}
}

// inBatches makes the listing of dir arrive n names per directory read, with
// extra names after the real ones and then fail, if fail is set, instead of
// ending. hold, if set, runs before the read that would return batch number
// holdAt (1-based) and may block it.
type inBatches struct {
	dir     string
	n       int
	extra   []string
	fail    error
	holdAt  int
	hold    func()
	reads   atomic.Int32
	pending []string
	loaded  bool
}

func (b *inBatches) install(spy *scanSpy) {
	spy.readNames = func(key string, call func() ([]string, error)) ([]string, error) {
		if key != b.dir {
			return call()
		}
		// The listing's reads are sequential: the scan's own, then its reader's.
		read := int(b.reads.Add(1))
		if !b.loaded {
			b.loaded = true
			for {
				names, err := call()
				b.pending = append(b.pending, names...)
				if err == io.EOF {
					break
				}
				if err != nil {
					return nil, err
				}
			}
			b.pending = append(b.pending, b.extra...)
		}
		if b.hold != nil && read == b.holdAt {
			b.hold()
		}
		if len(b.pending) == 0 {
			if b.fail != nil {
				return nil, b.fail
			}
			return nil, io.EOF
		}
		batch := b.pending[:min(b.n, len(b.pending))]
		b.pending = b.pending[len(batch):]
		return batch, nil
	}
}

// Files of a long directory leave in pages while it is still being read,
// under a cursor that keeps its position before the directory's names.
func TestLocalScanEmitsFilesWhileTheListingIsOpen(t *testing.T) {
	local, root := localFixture(t)
	writeFiles(t, root, numbered("big/f", 40)...)
	spy := spyOnScan(local, root)
	local.scanTuning.streamAfter = 8
	release := make(chan struct{})
	// The scan reads 10 names and streams; its reader's first read waits
	// for the first page.
	listing := &inBatches{dir: "big", n: 10, holdAt: 2, hold: func() { <-release }}
	listing.install(spy)
	var first *Page
	var got []string
	err := local.ScanPages(context.Background(), ListOptions{Limit: 5}, func(page Page) error {
		if first == nil {
			first = &page
			close(release)
		}
		for _, object := range page.Objects {
			got = append(got, object.Key)
		}
		return nil
	})
	if err != nil || first == nil {
		t.Fatal(err)
	}
	if !first.Streamed || len(first.Objects) != 4 || !strings.HasPrefix(first.Objects[0].Key, "big/") {
		t.Fatalf("first page while the listing was open: %+v", first)
	}
	if frames := decodeFrames(t, local, first.NextCursor); len(frames) != 2 || frames[1].Name != "big" || frames[1].After != "" {
		t.Fatalf("streamed position moved: %+v", frames)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, numbered("big/f", 40)) {
		t.Fatalf("objects: %v", got)
	}
}

// With lookups held for the end of a listing (the release gate's fallback),
// no entry of a streamed directory is looked up while it is still being read
// and the buffer has room.
func TestLocalScanCanHoldLookupsUntilTheListingEnds(t *testing.T) {
	local, root := localFixture(t)
	writeFiles(t, root, numbered("big/f", 40)...)
	spy := spyOnScan(local, root)
	local.scanTuning.streamAfter, local.scanTuning.holdLookups = 8, true
	atEnd, release := make(chan struct{}), make(chan struct{})
	// Four reads return the names; the fifth, the end, waits.
	listing := &inBatches{dir: "big", n: 10, holdAt: 5, hold: func() { close(atEnd); <-release }}
	listing.install(spy)
	done := make(chan error, 1)
	go func() {
		_, err := scanAll(local, context.Background(), ListOptions{})
		done <- err
	}()
	<-atEnd
	lookedUp := 0
	for _, name := range numbered("big/f", 40) {
		lookedUp += spy.called("lstat", name)
	}
	close(release)
	if err := <-done; err != nil || lookedUp != 0 {
		t.Fatalf("looked up %d entries during the listing: %v", lookedUp, err)
	}
	// big, its 40 files, and big's link as it is left.
	if n := spy.total("lstat"); n != 42 {
		t.Fatalf("%d lookups in all", n)
	}
}

// A listing that fails after it began to stream fails the scan once every
// name read before the failure is done; the error names the directory.
func TestLocalScanReportsAStreamedListingFailure(t *testing.T) {
	local, root := localFixture(t)
	writeFiles(t, root, numbered("big/f", 10)...)
	spy := spyOnScan(local, root)
	local.scanTuning.streamAfter = 8
	failure := errors.New("listing interrupted")
	listing := &inBatches{dir: "big", n: 10, fail: &fs.PathError{Op: "readdirent", Path: filepath.Join(root, "big"), Err: failure}}
	listing.install(spy)
	pages, err := scanAll(local, context.Background(), ListOptions{Limit: 3})
	var pathError *fs.PathError
	if !errors.Is(err, failure) || !errors.As(err, &pathError) || pathError.Path != "big" {
		t.Fatalf("scan returned %v", err)
	}
	// big and two files, then three files twice; the last two files wait for
	// a page that the failure ends.
	if got := len(objectKeys(pages)); len(pages) != 3 || got != 8 {
		t.Fatalf("%d pages with %d objects before the failure", len(pages), got)
	}
}

// Cancelling a streaming scan waits for a directory read in progress before
// its listing is closed.
func TestLocalScanCancellationWaitsForTheReader(t *testing.T) {
	local, root := localFixture(t)
	writeFiles(t, root, numbered("big/f", 20)...)
	spy := spyOnScan(local, root)
	local.scanTuning.streamAfter = 8
	entered, release := make(chan struct{}), make(chan struct{})
	var reads atomic.Int32
	var heldErr error
	spy.readNames = func(key string, call func() ([]string, error)) ([]string, error) {
		if key != "big" || reads.Add(1) != 2 {
			return call()
		}
		// The reader's first read: the directory must still be open when
		// it goes on after the scan was cancelled.
		close(entered)
		<-release
		names, err := call()
		if err != io.EOF {
			heldErr = err
		}
		return names, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan error, 1)
	go func() {
		_, err := scanAll(local, ctx, ListOptions{})
		returned <- err
	}()
	<-entered
	cancel()
	select {
	case err := <-returned:
		t.Fatalf("scan returned %v with a directory read in progress", err)
	default:
	}
	close(release)
	if err := <-returned; !errors.Is(err, context.Canceled) || heldErr != nil {
		t.Fatalf("cancelled scan: %v, the held read found %v", err, heldErr)
	}
}

// A name the listing repeats is emitted once.
func TestLocalScanEmitsARepeatedNameOnce(t *testing.T) {
	local, root := localFixture(t)
	writeFiles(t, root, numbered("big/f", 20)...)
	spy := spyOnScan(local, root)
	local.scanTuning.streamAfter = 8
	listing := &inBatches{dir: "big", n: 10, extra: []string{"f0001", "f0003"}}
	listing.install(spy)
	pages, err := scanAll(local, context.Background(), ListOptions{})
	keys := objectKeys(pages)
	sort.Strings(keys)
	if err != nil || !reflect.DeepEqual(keys, numbered("big/f", 20)) {
		t.Fatalf("objects %v, %v", keys, err)
	}
}

// While a directory streams, the activity says so and counts what was
// examined in it; its deferred subdirectories are then entered sorted.
func TestLocalScanReportsStreamingActivity(t *testing.T) {
	local, root := localFixture(t)
	writeFiles(t, root, numbered("big/f", 20)...)
	writeFiles(t, root, "big/sub/x")
	spy := spyOnScan(local, root)
	local.scanTuning.streamAfter = 8
	listing := &inBatches{dir: "big", n: 10}
	listing.install(spy)
	activity := NewScanActivity()
	snapshots := map[string]*ScanActivitySnapshot{}
	var mu sync.Mutex
	spy.lstat = func(key string, call func() (fs.FileInfo, error)) (fs.FileInfo, error) {
		if key == "big/f0019" || key == "big/sub/x" {
			mu.Lock()
			snapshots[key] = activity.Snapshot()
			mu.Unlock()
		}
		return call()
	}
	if _, err := scanAll(local, context.Background(), ListOptions{Activity: activity}); err != nil {
		t.Fatal(err)
	}
	if s := snapshots["big/f0019"]; s == nil || s.Mode != ScanModeStreaming || s.Directory.Key != "big" {
		t.Fatalf("activity while big streams: %+v", s)
	}
	if s := snapshots["big/sub/x"]; s == nil || s.Mode != ScanModeSorted || s.Directory.Key != "big/sub" {
		t.Fatalf("activity in a deferred subdirectory: %+v", s)
	}
	after := activity.Snapshot()
	if after.NamesRead != 1+21+1 || after.Examined != 1+21+1 {
		t.Fatalf("activity after the scan: %+v", after)
	}
}

// The read-ahead buffer counts the names not yet consumed, and frees them as
// the scan consumes them.
func TestNameStreamBoundsItsBuffer(t *testing.T) {
	stream := &nameStream{limit: 3 * nameBytes("name"), arrived: make(chan struct{}, 1), room: make(chan struct{}, 1)}
	stream.add([]string{"name", "name"})
	if stream.full() {
		t.Fatal("buffer full below its limit")
	}
	stream.add([]string{"name"})
	if !stream.full() {
		t.Fatal("buffer not full at its limit")
	}
	stream.release(2)
	if stream.full() || stream.at(2) != "name" || stream.base != 2 || stream.bytes != nameBytes("name") {
		t.Fatalf("release kept %d bytes from %d", stream.bytes, stream.base)
	}
	if available, done, err := stream.available(); available != 3 || done || err != nil {
		t.Fatalf("available %d %v %v", available, done, err)
	}
}

// A streamed directory keeps its mark across restarts: while the scan is in
// one of its deferred subdirectories, the scan has not read it again, and
// once it has, it may no longer be long enough to stream. Every page whose
// cursor names it says Streamed, and every object is committed once.
func TestLocalScanKeepsTheStreamingMarkAcrossRestarts(t *testing.T) {
	for _, shrink := range []bool{false, true} {
		t.Run(fmt.Sprint("shrink=", shrink), func(t *testing.T) {
			root := t.TempDir()
			writeFiles(t, root, numbered("big/f", 20)...)
			writeFiles(t, root, numbered("big/dir/g", 6)...)
			writeFiles(t, root, "z")
			ctx, stop := context.Background(), errors.New("saved")
			committed := &committedKeys{keys: map[string]int{}}
			options := ListOptions{Limit: 2, Done: committed.done}
			page := func() (Page, []localScanFrame) {
				t.Helper()
				local := streamingLocal(t, root, localScanTuning{streamAfter: 8, earlyFlush: 8})
				defer local.Close()
				var saved Page
				if err := local.ScanPages(ctx, options, func(p Page) error { saved = p; return stop }); !errors.Is(err, stop) {
					t.Fatal(err)
				}
				committed.commit(saved)
				options.Cursor = saved.NextCursor
				return saved, decodeFrames(t, local, saved.NextCursor)
			}
			for {
				saved, frames := page()
				if saved.NextCursor == "" {
					t.Fatal("the scan never saved a position inside big/dir")
				}
				if len(frames) == 3 && frames[2].Name == "dir" {
					if !saved.Streamed || !frames[1].Streamed {
						t.Fatalf("page inside big/dir: %+v under %+v", saved, frames)
					}
					break
				}
			}
			if shrink {
				// big now lists 7 names, too few to stream when the scan
				// returns to it.
				for _, name := range numbered("f", 14) {
					if err := os.Remove(filepath.Join(root, "big", name)); err != nil {
						t.Fatal(err)
					}
				}
			}
			inside, after := 0, 0
			for options.Cursor != "" {
				saved, frames := page()
				onPath := len(frames) > 1 && frames[1].Name == "big"
				if saved.Streamed != onPath || onPath && !frames[1].Streamed {
					t.Fatalf("resumed page %+v under %+v", saved, frames)
				}
				if onPath {
					inside++
				} else {
					after++
				}
			}
			if inside == 0 || after == 0 {
				t.Fatalf("%d resumed pages named big, %d did not", inside, after)
			}
			for key, n := range committed.keys {
				if n != 1 {
					t.Fatalf("%s committed %d times", key, n)
				}
			}
		})
	}
}

// A name the listing repeats after the page holding its first copy was
// handed on is neither looked up nor emitted again, although Done was asked
// about the repeat before that page left: the scan asks again after every
// page. A second lookup could find the file changed and fail the scan.
func TestLocalScanAsksDoneAgainAfterAStreamedPage(t *testing.T) {
	local, root := localFixture(t)
	writeFiles(t, root, numbered("big/f", 20)...)
	spy := spyOnScan(local, root)
	local.scanTuning.streamAfter = 8
	var pending []string
	loaded := false
	spy.readNames = func(key string, call func() ([]string, error)) ([]string, error) {
		if key != "big" {
			return call()
		}
		for !loaded {
			names, err := call()
			pending = append(pending, names...)
			if err == io.EOF {
				loaded = true
				// The listing repeats f0001 six names on, as an NFS listing
				// resumed at a reused cookie does: within the first read.
				pending = append(append(append([]string{}, pending[:6]...), "f0001"), pending[6:]...)
			} else if err != nil {
				return nil, err
			}
		}
		if len(pending) == 0 {
			return nil, io.EOF
		}
		batch := pending[:min(10, len(pending))]
		pending = pending[len(batch):]
		return batch, nil
	}
	committed := &committedKeys{keys: map[string]int{}}
	emitted := 0
	err := local.ScanPages(context.Background(), ListOptions{Limit: 2, Done: committed.done}, func(page Page) error {
		committed.commit(page)
		for _, object := range page.Objects {
			if object.Key == "big/f0001" {
				emitted++
			}
		}
		return nil
	})
	if err != nil || emitted != 1 || spy.called("lstat", "big/f0001") != 1 || len(committed.keys) != 20 {
		t.Fatalf("big/f0001 emitted %d times after %d lookups, %d objects: %v", emitted, spy.called("lstat", "big/f0001"), len(committed.keys), err)
	}
}

// Once a streamed listing has ended, the directory shows as being examined,
// entry x of the names read, and no longer as being read.
func TestLocalScanShowsAStreamedListingAsReadOnceItEnds(t *testing.T) {
	local, root := localFixture(t)
	writeFiles(t, root, numbered("big/f", 20)...)
	spy := spyOnScan(local, root)
	local.scanTuning.streamAfter = 8
	activity := NewScanActivity()
	var open *ScanActivitySnapshot
	// The read that ends the listing finds it still being read.
	listing := &inBatches{dir: "big", n: 10, holdAt: 3, hold: func() { open = activity.Snapshot() }}
	listing.install(spy)
	release := make(chan struct{})
	spy.lstat = func(key string, call func() (fs.FileInfo, error)) (fs.FileInfo, error) {
		if strings.HasPrefix(key, "big/") {
			<-release
		}
		return call()
	}
	done := make(chan error, 1)
	go func() {
		_, err := scanAll(local, context.Background(), ListOptions{Activity: activity})
		done <- err
	}()
	// No entry of big can be consumed meanwhile, so the scan still streams.
	var listed *ScanActivitySnapshot
	for deadline := time.Now().Add(time.Minute); listed == nil; time.Sleep(time.Millisecond) {
		if s := activity.Snapshot(); s != nil && s.Mode == ScanModeStreaming && s.Directory != nil && !s.Directory.Reading {
			listed = s
		} else if time.Now().After(deadline) {
			t.Fatal("hang guard: the ended listing still shows as being read")
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if open == nil || open.Directory == nil || !open.Directory.Reading || open.Directory.Key != "big" {
		t.Fatalf("activity while the listing was open: %+v", open)
	}
	if d := listed.Directory; d.Key != "big" || d.Names != 20 || d.Position != 1 {
		t.Fatalf("activity once the listing ended: %+v", d)
	}
}

// A subdirectory deferred while its parent streamed is entered with the
// identity its lookup found: if it cannot be opened as that directory, one
// fresh lookup decides, as for any other subdirectory.
func TestLocalScanChildRuleForDeferredSubdirectories(t *testing.T) {
	injected := errors.New("open refused")
	for _, test := range []struct {
		name   string
		change func(t *testing.T, root string, call func() (*os.Root, error)) (*os.Root, error)
		want   []string
		err    error
	}{
		{"replaced by a directory before the open", func(t *testing.T, root string, call func() (*os.Root, error)) (*os.Root, error) {
			if err := os.Rename(filepath.Join(root, "big", "sub"), filepath.Join(root, "moved")); err != nil {
				t.Fatal(err)
			}
			writeFiles(t, root, "big/sub/new")
			return call()
		}, []string{"big/sub/new"}, nil},
		{"vanished", func(t *testing.T, root string, call func() (*os.Root, error)) (*os.Root, error) {
			if err := os.RemoveAll(filepath.Join(root, "big", "sub")); err != nil {
				t.Fatal(err)
			}
			return call()
		}, nil, nil},
		{"the same directory refuses the open", func(*testing.T, string, func() (*os.Root, error)) (*os.Root, error) {
			return nil, injected
		}, nil, injected},
		{"another directory opened, the entry unchanged", func(t *testing.T, root string, _ func() (*os.Root, error)) (*os.Root, error) {
			return os.OpenRoot(filepath.Join(root, "other"))
		}, nil, ErrSourceChanged},
	} {
		t.Run(test.name, func(t *testing.T) {
			local, root := localFixture(t)
			writeFiles(t, root, numbered("big/f", 10)...)
			writeFiles(t, root, "big/sub/old", "other/o")
			spy := spyOnScan(local, root)
			local.scanTuning.streamAfter = 8
			spy.openRoot = func(key string, call func() (*os.Root, error)) (*os.Root, error) {
				if key == "big/sub" {
					return test.change(t, root, call)
				}
				return call()
			}
			pages, err := scanAll(local, context.Background(), ListOptions{})
			if test.err != nil {
				if !errors.Is(err, test.err) {
					t.Fatalf("got %v, want %v", err, test.err)
				}
				return
			}
			var got []string
			for _, key := range objectKeys(pages) {
				if strings.HasPrefix(key, "big/sub/") {
					got = append(got, key)
				}
			}
			if err != nil || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("got %v %v, want %v", got, err, test.want)
			}
		})
	}
}

// openRootHook is the scan's access to the source, calling hook before each
// directory it opens as a root, and keeping no record of its own.
type openRootHook struct {
	rootScanFS
	hook func(name string)
}

func (h openRootHook) OpenRoot(dir *os.Root, name string) (*os.Root, error) {
	h.hook(name)
	return dir.OpenRoot(name)
}

// While a streamed directory's subdirectories wait for the end of its
// listing and then for their turn, each costs its name and its identity, not
// the whole FileInfo of its lookup.
func TestLocalScanDefersSubdirectoriesCompactly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a Windows FileInfo does not export its file index, so a deferred subdirectory keeps it")
	}
	local, root := localFixture(t)
	const subdirectories = 8000
	for i := range subdirectories {
		if err := os.MkdirAll(filepath.Join(root, "big", fmt.Sprintf("d%05d", i)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	heap := func() uint64 {
		runtime.GC()
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		return stats.HeapAlloc
	}
	var during uint64
	local.scanTuning = &localScanTuning{streamAfter: 8, fs: openRootHook{hook: func(name string) {
		if name == "d00000" {
			// The first deferred subdirectory: every other one still waits.
			during = heap()
		}
	}}}
	before := heap()
	if _, err := scanAll(local, context.Background(), ListOptions{}); err != nil {
		t.Fatal(err)
	}
	if during == 0 {
		t.Fatal("big/d00000 was never entered")
	}
	// A lookup's FileInfo alone takes more than 200 bytes on a 64-bit Unix,
	// and with it each subdirectory took about 400; a name and an identity
	// take about 50.
	if during > before && during-before > subdirectories*250 {
		t.Fatalf("the deferred subdirectories took %d bytes, %d each", during-before, (during-before)/subdirectories)
	}
}

// A streamed listing's reader charges its reads as the scan does: the read
// that only confirms the end of the listing is free.
func TestLocalScanChargesTheStreamedReadsThatReturnNames(t *testing.T) {
	local, root := localFixture(t)
	writeFiles(t, root, numbered("big/f", 30)...)
	spy := spyOnScan(local, root)
	local.scanTuning.streamAfter = 8
	// Ten names a read: the scan's read streams, and its reader's three
	// further reads return 10, 10 and the end.
	listing := &inBatches{dir: "big", n: 10}
	listing.install(spy)
	limiter := &countingLocalIO{}
	local.SetLocalIOLimiter(limiter)
	pages, err := scanAll(local, context.Background(), ListOptions{})
	if err != nil || len(objectKeys(pages)) != 30 {
		t.Fatalf("scan: %d objects, %v", len(objectKeys(pages)), err)
	}
	// The root: its open and one read. big: its lookup, root and open, and
	// the three reads that return names, but not the one that ends the
	// listing. 30 lookups, and big's link as it is left.
	if operations, _ := limiter.take(); operations != 2+(3+3)+30+1+identityOpens(30) || listing.reads.Load() != 4 {
		t.Fatalf("scan charged %d operations for %d reads of big", operations, listing.reads.Load())
	}
}

// gatedLocalIO admits every local operation until it is armed, then holds
// each one until release is closed.
type gatedLocalIO struct {
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (g *gatedLocalIO) WaitLocalOperation(ctx context.Context) error {
	if g.armed.Load() {
		select {
		case g.entered <- struct{}{}:
		default:
		}
		select {
		case <-g.release:
		case <-ctx.Done():
		}
	}
	return ctx.Err()
}

func (g *gatedLocalIO) WaitLocalBytes(ctx context.Context, _ int) error { return ctx.Err() }

// A scan waiting for the names of a streamed listing, while the reader waits
// for the local operation limit, says that it waits for that limit.
func TestLocalScanShowsAStreamedReaderWaitingForTheLocalLimit(t *testing.T) {
	local, root := localFixture(t)
	writeFiles(t, root, numbered("big/f", 30)...)
	spy := spyOnScan(local, root)
	// Lookups wait for the listing's end, so while the reader waits nothing
	// but its read is charged.
	local.scanTuning.streamAfter, local.scanTuning.holdLookups = 8, true
	listing := &inBatches{dir: "big", n: 10}
	listing.install(spy)
	gate := &gatedLocalIO{entered: make(chan struct{}, 1), release: make(chan struct{})}
	local.SetLocalIOLimiter(gate)
	read := spy.readNames
	spy.readNames = func(key string, call func() ([]string, error)) ([]string, error) {
		names, err := read(key, call)
		if key == "big" && listing.reads.Load() == 1 {
			gate.armed.Store(true) // the scan's own read streams; the reader's first read waits
		}
		return names, err
	}
	activity := NewScanActivity()
	result := make(chan error, 1)
	var objects int
	go func() {
		pages, err := scanAll(local, context.Background(), ListOptions{Activity: activity})
		objects = len(objectKeys(pages))
		result <- err
	}()
	select {
	case <-gate.entered:
	case err := <-result:
		t.Fatalf("the scan ended before its reader waited: %v", err)
	}
	// Hang guard only: the scan reaches its wait for names soon after.
	for deadline := time.Now().Add(30 * time.Second); activity.Snapshot().WaitingFor != ScanWaitLocalLimit; {
		if time.Now().After(deadline) {
			t.Fatalf("waiting for %q while the reader waits for the local limit", activity.Snapshot().WaitingFor)
		}
		time.Sleep(time.Millisecond)
	}
	gate.armed.Store(false)
	close(gate.release)
	if err := <-result; err != nil || objects != 30 {
		t.Fatalf("scan: %d objects, %v", objects, err)
	}
	if got := activity.Snapshot().WaitingFor; got != "" {
		t.Fatalf("still waiting for %q after the scan", got)
	}
}
