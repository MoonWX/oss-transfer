package transferstore

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// randomScanTree builds a small tree full of the cases a scan must order and
// classify exactly as before: a/0 against a.txt, links, FIFOs, probes and
// staging namespaces, and, if invalid is set, a name no object key can have.
func randomScanTree(t *testing.T, seed int64, invalid bool) string {
	t.Helper()
	random := rand.New(rand.NewSource(seed))
	root := t.TempDir()
	// No two names differ only in case, which some file systems do not tell.
	names := []string{"0", "1", "a", "a.txt", "a-b", "b", "z", "子", "with space", "Q"}
	write := func(path string) {
		if err := os.WriteFile(path, []byte(fmt.Sprint(random.Int63())), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var build func(dir string, depth int)
	build = func(dir string, depth int) {
		for range random.Intn(6) {
			path := filepath.Join(dir, names[random.Intn(len(names))])
			if _, err := os.Lstat(path); err == nil {
				continue
			}
			switch kind := random.Intn(10); {
			case kind < 5:
				write(path)
			case kind < 8 && depth < 3:
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
				build(path, depth+1)
			case kind == 8 && runtime.GOOS != "windows":
				if err := os.Symlink(dir, path); err != nil {
					t.Fatal(err)
				}
			case kind == 9 && runtime.GOOS != "windows":
				if err := makeFIFO(path); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	build(root, 0)
	if err := os.MkdirAll(filepath.Join(root, "a"), 0700); err == nil {
		if _, err := os.Lstat(filepath.Join(root, "a", "0")); err != nil {
			write(filepath.Join(root, "a", "0"))
		}
		if _, err := os.Lstat(filepath.Join(root, "a.txt")); err != nil {
			write(filepath.Join(root, "a.txt"))
		}
	}
	// A real probe is hidden; a file that only shares its prefix is not.
	if _, err := CreateSharedProbe(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(root, sharedProbePrefix+"lookalike"))
	parent, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	namespace, err := CreateLocalTempNamespace(context.Background(), parent, root, strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(root, namespace.Name, "partial"))
	unmarked := filepath.Join(root, namespacePrefix+strings.Repeat("cd", 32)+"-"+strings.Repeat("01", 16))
	if err := os.Mkdir(unmarked, 0700); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(unmarked, "kept"))
	if invalid && runtime.GOOS != "windows" {
		// In the root, so that every scope reaches it.
		write(filepath.Join(root, `back\slash`))
	}
	return root
}

type pageScan func(*Local, context.Context, ListOptions, func(Page) error) error

func headScan(local *Local, ctx context.Context, options ListOptions, emit func(Page) error) error {
	return local.headScanPages(ctx, options, emit)
}

func concurrentScan(local *Local, ctx context.Context, options ListOptions, emit func(Page) error) error {
	return local.ScanPages(ctx, options, emit)
}

// scanEveryPage runs a scan to its end, reopening the root and resuming from
// the saved cursor after every page when reopen is set.
func scanEveryPage(t *testing.T, root string, options ListOptions, reopen bool, scan pageScan) ([]Page, error) {
	t.Helper()
	var pages []Page
	stop := errors.New("page saved")
	for {
		local, err := NewLocal(root)
		if err != nil {
			t.Fatal(err)
		}
		err = scan(local, context.Background(), options, func(page Page) error {
			pages = append(pages, page)
			if reopen {
				return stop
			}
			return nil
		})
		_ = local.Close()
		if !reopen || !errors.Is(err, stop) {
			return pages, err
		}
		if options.Cursor = pages[len(pages)-1].NextCursor; options.Cursor == "" {
			return pages, nil
		}
		if len(pages) > 1000 {
			t.Fatal("scan made no progress")
		}
	}
}

// The concurrent scan returns the released scan's pages and cursors byte for
// byte, through every combination of scope, page size and resumption.
func TestLocalScanMatchesReleasedPagesAndCursors(t *testing.T) {
	scopes := []ListOptions{{}, {Prefix: "a"}, {Prefix: "a/"}, {StartAfter: "a.txt"}, {Prefix: "b", StartAfter: "b/0"}}
	for seed := range int64(12) {
		invalid := seed%4 == 3
		root := randomScanTree(t, seed, invalid)
		for _, scope := range scopes {
			for limit := 1; limit <= 7; limit++ {
				for _, reopen := range []bool{false, true} {
					options := scope
					options.Limit = limit
					want, wantErr := scanEveryPage(t, root, options, reopen, headScan)
					got, gotErr := scanEveryPage(t, root, options, reopen, concurrentScan)
					if fmt.Sprint(gotErr) != fmt.Sprint(wantErr) || !reflect.DeepEqual(got, want) {
						t.Fatalf("seed %d %+v reopen=%v:\ngot  %d pages, %v\nwant %d pages, %v", seed, options, reopen, len(got), gotErr, len(want), wantErr)
					}
					if invalid && runtime.GOOS != "windows" && scope.Prefix != "b" && !errors.Is(gotErr, ErrInvalidKey) {
						t.Fatalf("seed %d %+v: invalid name not reported: %v", seed, options, gotErr)
					}
				}
			}
		}
	}
}

// Without invalid names, the pages hold exactly what Scan streams, each object
// as Stat reports it.
func TestLocalScanPagesEqualScanAndStat(t *testing.T) {
	for seed := range int64(6) {
		root := randomScanTree(t, seed, false)
		local, err := NewLocal(root)
		if err != nil {
			t.Fatal(err)
		}
		defer local.Close()
		for _, scope := range []ListOptions{{}, {Prefix: "a"}, {StartAfter: "a.txt"}} {
			var want []Object
			if err := local.Scan(context.Background(), scope, func(object Object) error { want = append(want, object); return nil }); err != nil {
				t.Fatal(err)
			}
			options := scope
			options.Limit = 3
			pages, err := scanEveryPage(t, root, options, true, concurrentScan)
			if err != nil {
				t.Fatal(err)
			}
			var got []Object
			for _, page := range pages {
				got = append(got, page.Objects...)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("seed %d %+v: pages differ from Scan:\ngot  %+v\nwant %+v", seed, scope, got, want)
			}
			for _, object := range got {
				stat, err := local.Stat(context.Background(), object.Key)
				if err != nil || !reflect.DeepEqual(stat, object) {
					t.Fatalf("seed %d: %s differs from Stat: %+v %+v %v", seed, object.Key, object, stat, err)
				}
			}
		}
	}
}
