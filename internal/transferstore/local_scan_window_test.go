package transferstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// scanSpy is a localScanFS that counts calls by the key they reach and lets a
// test step into any of them. Keys are relative to the store's root.
type scanSpy struct {
	root      string
	mu        sync.Mutex
	calls     map[string]int
	lstat     func(key string, call func() (fs.FileInfo, error)) (fs.FileInfo, error)
	openRoot  func(key string, call func() (*os.Root, error)) (*os.Root, error)
	readNames func(key string, call func() ([]string, error)) ([]string, error)
}

func spyOnScan(local *Local, root string) *scanSpy {
	spy := &scanSpy{root: root, calls: map[string]int{}}
	local.scanTuning = &localScanTuning{fs: spy}
	return spy
}

func (s *scanSpy) key(directory, name string) string {
	relative, err := filepath.Rel(s.root, directory)
	if err != nil {
		panic(err)
	}
	return path.Join(filepath.ToSlash(relative), name)
}

func (s *scanSpy) count(operation, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[operation+" "+key]++
}

func (s *scanSpy) called(operation, key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[operation+" "+key]
}

func (s *scanSpy) total(operation string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for call, n := range s.calls {
		if strings.HasPrefix(call, operation+" ") {
			total += n
		}
	}
	return total
}

func (s *scanSpy) Lstat(dir *os.Root, name string) (fs.FileInfo, error) {
	key := s.key(dir.Name(), name)
	s.count("lstat", key)
	call := func() (fs.FileInfo, error) { return dir.Lstat(name) }
	if s.lstat != nil {
		return s.lstat(key, call)
	}
	return call()
}

func (s *scanSpy) OpenRoot(dir *os.Root, name string) (*os.Root, error) {
	key := s.key(dir.Name(), name)
	s.count("openRoot", key)
	call := func() (*os.Root, error) { return dir.OpenRoot(name) }
	if s.openRoot != nil {
		return s.openRoot(key, call)
	}
	return call()
}

func (s *scanSpy) OpenDir(dir *os.Root) (*os.File, error) {
	s.count("openDir", s.key(dir.Name(), "."))
	return dir.Open(".")
}

func (s *scanSpy) ReadNames(file *os.File, n int) ([]string, error) {
	key := s.key(file.Name(), ".")
	s.count("read", key)
	call := func() ([]string, error) { return file.Readdirnames(n) }
	if s.readNames != nil {
		return s.readNames(key, call)
	}
	return call()
}

func writeFiles(t *testing.T, directory string, names ...string) {
	t.Helper()
	for _, name := range names {
		path := filepath.Join(directory, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func numbered(prefix string, n int) []string {
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("%s%04d", prefix, i)
	}
	return names
}

func scanAll(local *Local, ctx context.Context, options ListOptions) ([]Page, error) {
	var pages []Page
	err := local.ScanPages(ctx, options, func(page Page) error {
		pages = append(pages, page)
		return nil
	})
	return pages, err
}

func objectKeys(pages []Page) []string {
	var keys []string
	for _, page := range pages {
		for _, object := range page.Objects {
			keys = append(keys, object.Key)
		}
	}
	return keys
}

// A directory is read by names alone, 1,024 at a time up to the end, and the
// entries of a flat directory are then looked up exactly once each, however
// the pages fall. Only results parked above a page that ends below them are
// looked up again, and directories once more whenever their link is checked.
func TestLocalScanReadsNamesOnlyAndLooksUpEachEntryOnce(t *testing.T) {
	local, root := localFixture(t)
	writeFiles(t, root, numbered("d/f", 2100)...)
	writeFiles(t, root, "e.txt", "sub/x")
	spy := spyOnScan(local, root)
	pages, err := scanAll(local, context.Background(), ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 3 || len(objectKeys(pages)) != 2102 {
		t.Fatalf("scan returned %d pages and %d objects", len(pages), len(objectKeys(pages)))
	}
	for _, file := range append(numbered("d/f", 2100), "sub/x") {
		if n := spy.called("lstat", file); n != 1 {
			t.Fatalf("%s was looked up %d times", file, n)
		}
	}
	// e.txt and sub were looked up alongside d, then dropped when the first
	// page ended inside d. d: its lookup, its link before and after the
	// callbacks of the two pages that end inside it, and as it is left with
	// objects in the open page. sub: twice, and as it is left.
	for key, want := range map[string]int{"lstat e.txt": 2, "lstat d": 6, "lstat sub": 3, "read .": 2, "read d": 4, "read sub": 2,
		"openDir .": 1, "openDir d": 1, "openDir sub": 1, "openRoot d": 1, "openRoot sub": 1} {
		operation, path, _ := strings.Cut(key, " ")
		if n := spy.called(operation, path); n != want {
			t.Errorf("%s: %d calls, want %d", key, n, want)
		}
	}
	if n := spy.total("lstat"); n != 2101+2+6+3 {
		t.Fatalf("%d lookups in all", n)
	}
}

// Lookups finish in any order; entries are still consumed in name order, with
// the whole window in flight at once.
func TestLocalScanKeepsNameOrderWhateverOrderLookupsFinish(t *testing.T) {
	local, root := localFixture(t)
	names := numbered("f", 40)
	writeFiles(t, root, names...)
	spy := spyOnScan(local, root)
	gates := map[string]chan struct{}{}
	for _, name := range names {
		gates[name] = make(chan struct{})
	}
	arrived := make(chan string, len(names))
	var blocked, most atomic.Int32
	spy.lstat = func(key string, call func() (fs.FileInfo, error)) (fs.FileInfo, error) {
		now := blocked.Add(1)
		for {
			if old := most.Load(); now <= old || most.CompareAndSwap(old, now) {
				break
			}
		}
		arrived <- key
		<-gates[key]
		blocked.Add(-1)
		return call()
	}
	result := make(chan []Page, 1)
	go func() {
		pages, err := scanAll(local, context.Background(), ListOptions{})
		if err != nil {
			t.Error(err)
		}
		result <- pages
	}()
	var first []string
	for range localScanWindow {
		first = append(first, <-arrived)
	}
	for i := len(first) - 1; i >= 0; i-- {
		close(gates[first[i]])
	}
	for range len(names) - localScanWindow {
		close(gates[<-arrived])
	}
	pages := <-result
	if got := objectKeys(pages); !reflect.DeepEqual(got, names) {
		t.Fatalf("objects out of name order: %v", got)
	}
	if most.Load() != localScanWindow {
		t.Fatalf("%d lookups were in flight at once, want %d", most.Load(), localScanWindow)
	}
}

// within returns the next value on signal. The timeout is a hang guard only:
// it fails a scan that never sends it.
func within[T any](t *testing.T, signal <-chan T, what string) T {
	t.Helper()
	select {
	case value := <-signal:
		return value
	case <-time.After(time.Minute):
		t.Fatalf("hang guard: %s", what)
		var zero T
		return zero
	}
}

// While the head's lookup waits, each lookup that finishes behind it frees
// its slot for the next name: the window stays full instead of draining.
func TestLocalScanRefillsTheWindowWhileTheHeadWaits(t *testing.T) {
	local, root := localFixture(t)
	names := numbered("f", 100)
	writeFiles(t, root, names...)
	spy := spyOnScan(local, root)
	gates := map[string]chan struct{}{}
	for _, name := range names {
		gates[name] = make(chan struct{})
	}
	arrived := make(chan string, len(names))
	var blocked, most atomic.Int32
	spy.lstat = func(key string, call func() (fs.FileInfo, error)) (fs.FileInfo, error) {
		now := blocked.Add(1)
		for {
			if old := most.Load(); now <= old || most.CompareAndSwap(old, now) {
				break
			}
		}
		arrived <- key
		<-gates[key]
		blocked.Add(-1)
		return call()
	}
	result := make(chan []Page, 1)
	go func() {
		pages, err := scanAll(local, context.Background(), ListOptions{})
		if err != nil {
			t.Error(err)
		}
		result <- pages
	}()
	first := map[string]bool{}
	for range localScanWindow {
		first[within(t, arrived, "the window was not filled")] = true
	}
	// The head, f0000, still waits; one lookup behind it finishes.
	opened := map[string]bool{"f0001": true}
	close(gates["f0001"])
	next := within(t, arrived, "no lookup was issued while the head waited")
	if first[next] || next != "f0032" {
		t.Fatalf("issued %s while the head waited, want f0032", next)
	}
	opened[next] = true
	close(gates[next])
	for _, name := range names {
		if !opened[name] {
			close(gates[name])
		}
	}
	pages := within(t, result, "the scan did not end")
	if got := objectKeys(pages); !reflect.DeepEqual(got, names) {
		t.Fatalf("objects out of name order: %v", got)
	}
	if most.Load() != localScanWindow {
		t.Fatalf("%d lookups were in flight at once, want %d", most.Load(), localScanWindow)
	}
}

// Behind a head that waits, results park up to the last name the open page
// can reach, and no further: each entry is still looked up exactly once.
func TestLocalScanParksResultsUpToThePageBehindASlowHead(t *testing.T) {
	local, root := localFixture(t)
	names := numbered("f", 300)
	writeFiles(t, root, names...)
	spy := spyOnScan(local, root)
	release, reached := make(chan struct{}), make(chan struct{})
	var lookups atomic.Int32
	spy.lstat = func(key string, call func() (fs.FileInfo, error)) (fs.FileInfo, error) {
		if lookups.Add(1) == 100 {
			close(reached)
		}
		if key == "f0000" {
			<-release
		}
		return call()
	}
	var atFirstPage int
	result := make(chan error, 1)
	var pages []Page
	go func() {
		result <- local.ScanPages(context.Background(), ListOptions{Limit: 100}, func(page Page) error {
			if len(pages) == 0 {
				atFirstPage = spy.total("lstat")
			}
			pages = append(pages, page)
			return nil
		})
	}()
	within(t, reached, "results did not park up to the page while the head waited")
	close(release)
	if err := within(t, result, "the scan did not end"); err != nil {
		t.Fatal(err)
	}
	if atFirstPage != 100 || !reflect.DeepEqual(objectKeys(pages[:1]), names[:100]) {
		t.Fatalf("first page after %d lookups: %v", atFirstPage, objectKeys(pages[:1]))
	}
	if !reflect.DeepEqual(objectKeys(pages), names) {
		t.Fatal("objects out of name order")
	}
	for _, name := range names {
		if n := spy.called("lstat", name); n != 1 {
			t.Fatalf("%s was looked up %d times", name, n)
		}
	}
}

// Results a directory holds as the scan enters a subdirectory wait for the
// scan's return, and are not looked up again.
func TestLocalScanKeepsParkedResultsAcrossASubdirectory(t *testing.T) {
	local, root := localFixture(t)
	files := numbered("b", 60)
	writeFiles(t, root, files...)
	writeFiles(t, root, "a/x")
	spy := spyOnScan(local, root)
	release, reached := make(chan struct{}), make(chan struct{})
	var behind atomic.Int32
	spy.lstat = func(key string, call func() (fs.FileInfo, error)) (fs.FileInfo, error) {
		switch {
		case key == "a" && spy.called("lstat", "a") == 1:
			<-release
		case strings.HasPrefix(key, "b") && behind.Add(1) == int32(len(files)):
			close(reached)
		}
		return call()
	}
	result := make(chan []Page, 1)
	go func() {
		pages, err := scanAll(local, context.Background(), ListOptions{})
		if err != nil {
			t.Error(err)
		}
		result <- pages
	}()
	within(t, reached, "the files behind a slow directory were not looked up")
	close(release)
	pages := within(t, result, "the scan did not end")
	if got := objectKeys(pages); !reflect.DeepEqual(got, append([]string{"a/x"}, files...)) {
		t.Fatalf("objects: %v", got)
	}
	for _, name := range append([]string{"a/x"}, files...) {
		if n := spy.called("lstat", name); n != 1 {
			t.Fatalf("%s was looked up %d times", name, n)
		}
	}
}

// Of two failed lookups, the one earlier in name order is the scan's error,
// whichever finished first.
func TestLocalScanReportsTheEarliestFailure(t *testing.T) {
	local, root := localFixture(t)
	writeFiles(t, root, numbered("f", 10)...)
	spy := spyOnScan(local, root)
	early, late := errors.New("early failure"), errors.New("late failure")
	lateDone := make(chan struct{})
	spy.lstat = func(key string, call func() (fs.FileInfo, error)) (fs.FileInfo, error) {
		switch key {
		case "f0003":
			<-lateDone
			return nil, early
		case "f0007":
			defer close(lateDone)
			return nil, late
		}
		return call()
	}
	pages, err := scanAll(local, context.Background(), ListOptions{})
	if !errors.Is(err, early) || len(pages) != 0 {
		t.Fatalf("scan reported %v after %d pages", err, len(pages))
	}
}

// When a directory cannot be opened as the one its lookup found, one fresh
// lookup decides what happened to it.
func TestLocalScanChildRule(t *testing.T) {
	injected := errors.New("open refused")
	for _, test := range []struct {
		name   string
		change func(t *testing.T, root string, call func() (*os.Root, error)) (*os.Root, error)
		want   []string
		err    error
	}{
		{"vanished", func(t *testing.T, root string, call func() (*os.Root, error)) (*os.Root, error) {
			if err := os.RemoveAll(filepath.Join(root, "d")); err != nil {
				t.Fatal(err)
			}
			return call()
		}, []string{"other/o", "z"}, nil},
		{"replaced by a file", func(t *testing.T, root string, call func() (*os.Root, error)) (*os.Root, error) {
			if err := os.RemoveAll(filepath.Join(root, "d")); err != nil {
				t.Fatal(err)
			}
			writeFiles(t, root, "d")
			return call()
		}, nil, ErrSourceChanged},
		{"replaced by a directory before the open", func(t *testing.T, root string, call func() (*os.Root, error)) (*os.Root, error) {
			if err := os.Rename(filepath.Join(root, "d"), filepath.Join(root, "old")); err != nil {
				t.Fatal(err)
			}
			writeFiles(t, root, "d/new")
			return call()
		}, []string{"d/new", "other/o", "z"}, nil},
		{"the same directory refuses the open", func(*testing.T, string, func() (*os.Root, error)) (*os.Root, error) {
			return nil, injected
		}, nil, injected},
		{"another directory opened, the entry unchanged", func(t *testing.T, root string, _ func() (*os.Root, error)) (*os.Root, error) {
			return os.OpenRoot(filepath.Join(root, "other"))
		}, nil, ErrSourceChanged},
		{"replaced twice", func(t *testing.T, root string, call func() (*os.Root, error)) (*os.Root, error) {
			if err := os.Rename(filepath.Join(root, "d"), filepath.Join(root, "old")); err != nil {
				t.Fatal(err)
			}
			writeFiles(t, root, "d/first")
			opened, err := call()
			if err := os.Rename(filepath.Join(root, "d"), filepath.Join(root, "older")); err != nil {
				t.Fatal(err)
			}
			writeFiles(t, root, "d/second")
			return opened, err
		}, nil, ErrSourceChanged},
	} {
		t.Run(test.name, func(t *testing.T) {
			local, root := localFixture(t)
			writeFiles(t, root, "d/1", "d/2", "other/o", "z")
			spy := spyOnScan(local, root)
			spy.openRoot = func(key string, call func() (*os.Root, error)) (*os.Root, error) {
				if key == "d" {
					return test.change(t, root, call)
				}
				return call()
			}
			pages, err := scanAll(local, context.Background(), ListOptions{})
			if test.err != nil {
				if !errors.Is(err, test.err) || len(pages) != 0 {
					t.Fatalf("got %v after %d pages, want %v", err, len(pages), test.err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(objectKeys(pages), test.want) {
				t.Fatalf("got %v %v, want %v", objectKeys(pages), err, test.want)
			}
		})
	}
}

// Cancelling a scan returns its context's error only once the lookups in
// source calls have finished, and their directories stay open until then.
func TestLocalScanCancellationWaitsForItsLookups(t *testing.T) {
	local, root := localFixture(t)
	writeFiles(t, root, numbered("d/f", 40)...)
	spy := spyOnScan(local, root)
	release := make(chan struct{})
	arrived := make(chan struct{}, 40)
	var failed atomic.Int32
	spy.lstat = func(key string, call func() (fs.FileInfo, error)) (fs.FileInfo, error) {
		if !strings.HasPrefix(key, "d/") {
			return call()
		}
		arrived <- struct{}{}
		<-release
		info, err := call()
		if err != nil {
			failed.Add(1)
		}
		return info, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan error, 1)
	go func() {
		_, err := scanAll(local, ctx, ListOptions{})
		returned <- err
	}()
	for range localScanWindow {
		<-arrived
	}
	cancel()
	select {
	case err := <-returned:
		t.Fatalf("scan returned %v with its lookups still in source calls", err)
	default:
	}
	close(release)
	if err := <-returned; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled scan returned %v", err)
	}
	if failed.Load() != 0 {
		t.Fatalf("%d lookups found their directory closed under them", failed.Load())
	}
}

// Objects found through a directory's handle leave the scan only while the
// directory and all its ancestors are still linked where the cursor says.
func TestLocalScanChecksTheLinksOfEveryDirectoryOnThePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows refuses to rename a directory that a scan holds open, so its path cannot change under the scan")
	}
	replace := func(t *testing.T, root string) {
		t.Helper()
		if err := os.Rename(filepath.Join(root, "a"), filepath.Join(root, "moved")); err != nil {
			t.Fatal(err)
		}
		writeFiles(t, root, "a/b/c/1", "a/b/c/2")
	}
	t.Run("after the callback", func(t *testing.T) {
		local, root := localFixture(t)
		writeFiles(t, root, "a/b/c/1", "a/b/c/2")
		var got []string
		err := local.ScanPages(context.Background(), ListOptions{Limit: 4}, func(page Page) error {
			for _, object := range page.Objects {
				got = append(got, object.Key)
			}
			replace(t, root)
			return nil
		})
		if !errors.Is(err, ErrSourceChanged) || !reflect.DeepEqual(got, []string{"a/b/c/1"}) {
			t.Fatalf("scan went on through a replaced ancestor: %v %v", got, err)
		}
	})
	t.Run("before the callback", func(t *testing.T) {
		local, root := localFixture(t)
		writeFiles(t, root, "a/b/c/1", "a/b/c/2", "z")
		spy := spyOnScan(local, root)
		spy.lstat = func(key string, call func() (fs.FileInfo, error)) (fs.FileInfo, error) {
			info, err := call()
			if key == "a/b/c/2" {
				replace(t, root)
			}
			return info, err
		}
		pages, err := scanAll(local, context.Background(), ListOptions{})
		if !errors.Is(err, ErrSourceChanged) || len(pages) != 0 {
			t.Fatalf("page with objects from a replaced ancestor was emitted: %d pages, %v", len(pages), err)
		}
	})
	t.Run("as a directory is left", func(t *testing.T) {
		local, root := localFixture(t)
		writeFiles(t, root, "a/1", "b/1")
		spy := spyOnScan(local, root)
		spy.lstat = func(key string, call func() (fs.FileInfo, error)) (fs.FileInfo, error) {
			info, err := call()
			if key == "a/1" {
				if err := os.Rename(filepath.Join(root, "a"), filepath.Join(root, "moved")); err != nil {
					t.Fatal(err)
				}
				writeFiles(t, root, "a/1")
			}
			return info, err
		}
		pages, err := scanAll(local, context.Background(), ListOptions{})
		if !errors.Is(err, ErrSourceChanged) || len(pages) != 0 {
			t.Fatalf("objects of a replaced directory left the scan: %d pages, %v", len(pages), err)
		}
	})
}

// Objects already found are saved before a long child listing completes, at
// an ordinary position that resumes into the child.
func TestLocalScanSavesFoundObjectsBeforeALongListing(t *testing.T) {
	local, root := localFixture(t)
	writeFiles(t, root, "0")
	writeFiles(t, root, numbered("big/f", 10)...)
	spy := spyOnScan(local, root)
	local.scanTuning.earlyFlush = 4
	var first Page
	var readsAtFirstPage int
	stop := errors.New("first page saved")
	err := local.ScanPages(context.Background(), ListOptions{}, func(page Page) error {
		first, readsAtFirstPage = page, spy.called("read", "big")
		return stop
	})
	if !errors.Is(err, stop) || !reflect.DeepEqual(objectKeys([]Page{first}), []string{"0"}) || first.NextCursor == "" {
		t.Fatalf("first page %+v, %v", first, err)
	}
	if readsAtFirstPage != 1 {
		t.Fatalf("first page waited for %d reads of the long listing", readsAtFirstPage)
	}
	frames := decodeFrames(t, local, first.NextCursor)
	if len(frames) != 2 || frames[0].After != "big" || frames[1].Name != "big" || frames[1].After != "" {
		t.Fatalf("early page saved an unusual position: %+v", frames)
	}
	pages, err := scanAll(local, context.Background(), ListOptions{Cursor: first.NextCursor})
	if err != nil || !reflect.DeepEqual(objectKeys(pages), numbered("big/f", 10)) {
		t.Fatalf("resume after the early page: %v %v", objectKeys(pages), err)
	}
	// With no objects found yet, nothing is saved early.
	empty, emptyRoot := localFixture(t)
	writeFiles(t, emptyRoot, numbered("big/f", 10)...)
	spyOnScan(empty, emptyRoot)
	empty.scanTuning.earlyFlush = 4
	if pages, err := scanAll(empty, context.Background(), ListOptions{}); err != nil || len(pages) != 1 {
		t.Fatalf("scan without found objects saved early: %d pages, %v", len(pages), err)
	}
}

func decodeFrames(t *testing.T, local *Local, value string) []localScanFrame {
	t.Helper()
	frames, err := local.scanFrames(ListOptions{Cursor: value})
	if err != nil {
		t.Fatal(err)
	}
	return frames
}

// Keys the caller has already committed are neither looked up nor emitted.
// The caller is asked about 1,024 files at a time, and its failure is the
// scan's error once the scan reaches the first file it was asked about.
func TestLocalScanSkipsKeysTheCallerReportsDone(t *testing.T) {
	local, root := localFixture(t)
	names := numbered("f", 2100)
	writeFiles(t, root, names...)
	spy := spyOnScan(local, root)
	var asked []int
	done := func(keys []string) ([]bool, error) {
		asked = append(asked, len(keys))
		answers := make([]bool, len(keys))
		for i, key := range keys {
			answers[i] = strings.HasSuffix(key, "0") // every tenth
		}
		return answers, nil
	}
	pages, err := scanAll(local, context.Background(), ListOptions{Done: done})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(asked, []int{1024, 1024, 52}) {
		t.Fatalf("asked about %v keys", asked)
	}
	var want []string
	for _, name := range names {
		if !strings.HasSuffix(name, "0") {
			want = append(want, name)
		} else if n := spy.called("lstat", name); n != 0 {
			t.Fatalf("%s was reported done but looked up %d times", name, n)
		}
	}
	if !reflect.DeepEqual(objectKeys(pages), want) || spy.total("lstat") != len(want) {
		t.Fatalf("scan emitted %d objects after %d lookups, want %d", len(objectKeys(pages)), spy.total("lstat"), len(want))
	}
	failure := errors.New("index unavailable")
	calls := 0
	pages, err = scanAll(local, context.Background(), ListOptions{Done: func(keys []string) ([]bool, error) {
		if calls++; calls == 2 {
			return nil, failure
		}
		return make([]bool, len(keys)), nil
	}})
	if !errors.Is(err, failure) || len(pages) != 1 || len(pages[0].Objects) != 1000 {
		t.Fatalf("failed check reported %v after %d pages", err, len(pages))
	}
}

// The activity counts what this run did: names read, entries examined,
// entries already found and entries gone before they were examined.
func TestLocalScanReportsItsActivity(t *testing.T) {
	local, root := localFixture(t)
	writeFiles(t, root, "a/0", "a/1", "a/2", "b.txt", "c.txt", "d.txt")
	spy := spyOnScan(local, root)
	activity := NewScanActivity()
	var during *ScanActivitySnapshot
	spy.readNames = func(key string, call func() ([]string, error)) ([]string, error) {
		names, err := call()
		if key == "." && err == nil {
			if err := os.Remove(filepath.Join(root, "d.txt")); err != nil {
				t.Fatal(err)
			}
		}
		if key == "a" && err != nil {
			during = activity.Snapshot()
		}
		return names, err
	}
	done := func(keys []string) ([]bool, error) {
		answers := make([]bool, len(keys))
		for i, key := range keys {
			answers[i] = key == "b.txt"
		}
		return answers, nil
	}
	pages, err := scanAll(local, context.Background(), ListOptions{Done: done, Activity: activity})
	if err != nil || !reflect.DeepEqual(objectKeys(pages), []string{"a/0", "a/1", "a/2", "c.txt"}) {
		t.Fatalf("scan: %v %v", objectKeys(pages), err)
	}
	if during == nil || during.Directory == nil || !during.Directory.Reading || during.Directory.Key != "a" || during.Directory.Names != 3 || during.NamesRead != 7 {
		t.Fatalf("activity while reading a: %+v %+v", during, during.Directory)
	}
	after := activity.Snapshot()
	if after.Mode != ScanModeSorted || after.NamesRead != 7 || after.Examined != 5 || after.AlreadyFound != 1 || after.Vanished != 1 ||
		after.LookupMeanMillis == nil || after.OldestCallMillis != 0 || after.WaitingFor != "" {
		t.Fatalf("activity after the scan: %+v", after)
	}
	if d := after.Directory; d == nil || d.Key != "" || d.Reading || d.Names != 4 || d.Position != 4 {
		t.Fatalf("directory after the scan: %+v", d)
	}
}

// Each directory read is charged before it is made, except the read after a
// short one, which only confirms the end of the listing and is charged only
// if it returns names after all.
func TestLocalScanChargesTheReadsThatReturnNames(t *testing.T) {
	local, root := localFixture(t)
	writeFiles(t, root, numbered("d/f", 5)...)
	if err := os.Mkdir(filepath.Join(root, "e"), 0700); err != nil {
		t.Fatal(err)
	}
	spy := spyOnScan(local, root)
	// d's names arrive two at a time: each read is short, yet the listing
	// ends only with the read that returns none.
	var pending []string
	loaded := false
	spy.readNames = func(key string, call func() ([]string, error)) ([]string, error) {
		if key != "d" {
			return call()
		}
		for !loaded {
			names, err := call()
			pending = append(pending, names...)
			if err == io.EOF {
				loaded = true
			} else if err != nil {
				return nil, err
			}
		}
		if len(pending) == 0 {
			return nil, io.EOF
		}
		batch := pending[:min(2, len(pending))]
		pending = pending[len(batch):]
		return batch, nil
	}
	limiter := &countingLocalIO{}
	local.SetLocalIOLimiter(limiter)
	pages, err := scanAll(local, context.Background(), ListOptions{})
	if err != nil || !reflect.DeepEqual(objectKeys(pages), numbered("d/f", 5)) {
		t.Fatalf("scan: %v %v", objectKeys(pages), err)
	}
	// The root: its open and one read. d: its lookup, root and open, the
	// three reads that return names but not the fourth, five lookups, and its
	// link as it is left. e: its lookup, root and open, and one read: the
	// first read of a listing is charged even when it returns nothing.
	if operations, _ := limiter.take(); operations != 2+(3+3+5+1)+4+identityOpens(5) || spy.called("read", "d") != 4 {
		t.Fatalf("scan charged %d operations for %d reads of d", operations, spy.called("read", "d"))
	}
}

// Every source operation is charged to the local limits: a resumed path's
// links, roots and opens, each directory read, each lookup, a probe's open
// and bytes, and a staging namespace's open and marker.
func TestLocalScanChargesEveryOperation(t *testing.T) {
	local, root := localFixture(t)
	writeFiles(t, root, "a/0", "a/1", "a/2")
	var cursor string
	stop := errors.New("saved")
	if err := local.ScanPages(context.Background(), ListOptions{Limit: 2}, func(page Page) error { cursor = page.NextCursor; return stop }); !errors.Is(err, stop) {
		t.Fatal(err)
	}
	limiter := &countingLocalIO{}
	local.SetLocalIOLimiter(limiter)
	if _, err := scanAll(local, context.Background(), ListOptions{Limit: 2, Cursor: cursor}); err != nil {
		t.Fatal(err)
	}
	// The root's open; a's link, root and open; a's read (the read that
	// confirms the end of a short listing is free); two lookups; a's link as
	// it is left; the root's open and read.
	if operations, _ := limiter.take(); operations != 10+identityOpens(2) {
		t.Fatalf("resume charged %d operations", operations)
	}
	if _, err := CreateSharedProbe(context.Background(), filepath.Join(root, "a")); err != nil {
		t.Fatal(err)
	}
	parent, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	namespace, err := CreateLocalTempNamespace(context.Background(), parent, root, strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scanAll(local, context.Background(), ListOptions{}); err != nil {
		t.Fatal(err)
	}
	// The root: open, read, lookups of a and the namespace, the namespace's
	// root and marker. a: root, open, read, four lookups, the probe, and its
	// link as it is left.
	operations, n := limiter.take()
	marker := len(namespaceContents(namespace.Owner, namespace.ID))
	if operations != 2+2+2+3+4+1+1+identityOpens(3) || n != 94+marker {
		t.Fatalf("scan charged %d operations and %d bytes", operations, n)
	}
}
