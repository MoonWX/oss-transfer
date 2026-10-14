package transferstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func localFixture(t *testing.T) (*Local, string) {
	t.Helper()
	root := t.TempDir()
	s, err := NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, root
}
func writeLocal(t *testing.T, s *Local, key string, body []byte) {
	t.Helper()
	r, err := s.Write(context.Background(), key, bytes.NewReader(body), WriteOptions{Size: int64(len(body)), Overwrite: true})
	if err != nil || !r.Committed {
		t.Fatalf("write %s: %+v %v", key, r, err)
	}
}

func TestLocalIntegrityPaginationAndResume(t *testing.T) {
	s, root := localFixture(t)
	ctx := context.Background()
	keys := []string{"a/子文件", "a.txt", "dir/file name", "empty", "z"}
	data := bytes.Repeat([]byte("unaligned content 你好\x00"), 7011)
	for _, key := range keys {
		writeLocal(t, s, key, data)
	}
	var got []string
	options := ListOptions{Limit: 1}
	for {
		page, err := s.List(ctx, options)
		if err != nil {
			t.Fatal(err)
		}
		for _, obj := range page.Objects {
			got = append(got, obj.Key)
		}
		if page.NextCursor == "" {
			break
		}
		options.Cursor = page.NextCursor
	}
	if !reflect.DeepEqual(got, keys) {
		t.Fatalf("pagination lost/order changed: %v", got)
	}
	object, err := s.Stat(ctx, keys[0])
	if err != nil {
		t.Fatal(err)
	}
	reader, opened, err := s.Open(ctx, keys[0], OpenOptions{Expected: &object})
	if err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || sha256.Sum256(output) != sha256.Sum256(data) || opened.Size != int64(len(data)) {
		t.Fatalf("integrity: %v", err)
	}
	page, err := s.List(ctx, ListOptions{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	writeLocal(t, s, "new", []byte("new")) // Changes directory metadata; cursor must survive reopening.
	second, err := NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	resumed, err := second.List(ctx, ListOptions{Limit: 1, Cursor: page.NextCursor})
	if err != nil || len(resumed.Objects) != 1 || resumed.Objects[0].Key != "a.txt" {
		t.Fatalf("resume across reopen: %+v %v", resumed, err)
	}
	if _, err = s.List(ctx, ListOptions{Prefix: "a", Cursor: page.NextCursor}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("cursor accepted wrong prefix: %v", err)
	}
	var streamed []string
	if err = s.Scan(ctx, ListOptions{Prefix: "a"}, func(o Object) error { streamed = append(streamed, o.Key); return nil }); err != nil || !reflect.DeepEqual(streamed, keys[:2]) {
		t.Fatalf("scan %+v %v", streamed, err)
	}
	sentinel := errors.New("backpressure stop")
	count := 0
	err = s.Scan(ctx, ListOptions{}, func(Object) error { count++; return sentinel })
	if !errors.Is(err, sentinel) || count != 1 {
		t.Fatalf("scan ignored callback: %d %v", count, err)
	}
}

func TestLocalStartAfterIsExclusiveByteOrderAndCursorBound(t *testing.T) {
	s, _ := localFixture(t)
	ctx := context.Background()
	for _, key := range []string{"a/child", "a.txt", "b/child", "b.txt", "c"} {
		writeLocal(t, s, key, []byte(key))
	}
	// WalkDir visits a/child before a.txt, but the range is bytewise: a/child
	// must still be included. A range cannot be implemented by stopping early.
	want := []string{"a/child", "b/child", "b.txt", "c"}
	options := ListOptions{StartAfter: "a.txt", Limit: 1}
	var listed []string
	var firstCursor string
	for {
		page, err := s.List(ctx, options)
		if err != nil {
			t.Fatal(err)
		}
		for _, object := range page.Objects {
			listed = append(listed, object.Key)
		}
		if firstCursor == "" {
			firstCursor = page.NextCursor
		}
		if page.NextCursor == "" {
			break
		}
		options.Cursor = page.NextCursor
	}
	if !reflect.DeepEqual(listed, want) {
		t.Fatalf("range pages %v, want %v", listed, want)
	}
	var scanned []string
	err := s.Scan(ctx, ListOptions{StartAfter: "a.txt"}, func(o Object) error { scanned = append(scanned, o.Key); return nil })
	if err != nil || !reflect.DeepEqual(scanned, want) {
		t.Fatalf("range scan %v, %v", scanned, err)
	}
	scanned = nil
	err = s.Scan(ctx, ListOptions{StartAfter: "a.txt", Cursor: firstCursor}, func(o Object) error { scanned = append(scanned, o.Key); return nil })
	if err != nil || !reflect.DeepEqual(scanned, want[1:]) {
		t.Fatalf("range resumed scan %v, %v", scanned, err)
	}
	if _, err = s.List(ctx, ListOptions{Cursor: firstCursor}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("list accepted changed range: %v", err)
	}
	if err = s.Scan(ctx, ListOptions{StartAfter: "b.txt", Cursor: firstCursor}, func(Object) error { return nil }); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("scan accepted changed range: %v", err)
	}
}

func TestLocalPathConfinement(t *testing.T) {
	s, root := localFixture(t)
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", ".", "..", "../secret", "a/../secret", "/etc/passwd", "a\\b", "a\x00b", "a//b"} {
		if _, err := s.Write(context.Background(), key, strings.NewReader("x"), WriteOptions{Size: 1, Overwrite: true}); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("unsafe key %q: %v", key, err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"escape/secret", "escape/new"} {
		if _, err := s.Write(context.Background(), key, strings.NewReader("x"), WriteOptions{Size: 1, Overwrite: true}); err == nil {
			t.Errorf("escaped root write %q", key)
		}
		if reader, _, err := s.Open(context.Background(), key, OpenOptions{}); err == nil {
			reader.Close()
			t.Errorf("escaped root read %q", key)
		}
	}
	if err := os.Symlink(secret, filepath.Join(root, "final")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stat(context.Background(), "final"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("final symlink accepted: %v", err)
	}
	got, err := os.ReadFile(secret)
	if err != nil || string(got) != "untouched" {
		t.Fatalf("outside modified %q %v", got, err)
	}
	page, err := s.List(context.Background(), ListOptions{})
	if err != nil || len(page.Objects) != 0 {
		t.Fatalf("symlinks listed %+v %v", page, err)
	}
}

func TestLocalWriteFailurePreservesTarget(t *testing.T) {
	for _, input := range []string{"short", "too much input"} {
		t.Run(input, func(t *testing.T) {
			s, root := localFixture(t)
			writeLocal(t, s, "target", []byte("original"))
			result, err := s.Write(context.Background(), "target", strings.NewReader(input), WriteOptions{Size: 8, Overwrite: true})
			if !errors.Is(err, ErrSizeMismatch) || result.Committed {
				t.Fatalf("size mismatch %+v %v", result, err)
			}
			data, _ := os.ReadFile(filepath.Join(root, "target"))
			if string(data) != "original" {
				t.Fatalf("target truncated: %q", data)
			}
			entries, _ := os.ReadDir(root)
			if len(entries) != 1 {
				t.Fatalf("temporary leaked: %v", entries)
			}
		})
	}
	s, root := localFixture(t)
	writeLocal(t, s, "target", []byte("original"))
	pipe, writer := io.Pipe()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := s.Write(ctx, "target", pipe, WriteOptions{Size: 1024, Overwrite: true}); done <- err }()
	if _, err := writer.Write([]byte("partial")); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("blocked input was not interrupted")
	}
	data, _ := os.ReadFile(filepath.Join(root, "target"))
	if string(data) != "original" {
		t.Fatalf("cancel replaced target %q", data)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 {
		t.Fatalf("temporary leaked: %v", entries)
	}
}

type panicReader struct{}

func (panicReader) Read([]byte) (int, error) { panic("SKIP read source") }
func TestLocalAtomicSkipAndSourceChange(t *testing.T) {
	s, root := localFixture(t)
	ctx := context.Background()
	writeLocal(t, s, "target", []byte("original"))
	result, err := s.Write(ctx, "target", panicReader{}, WriteOptions{Size: 10})
	if err != nil || !result.Skipped || result.Committed {
		t.Fatalf("skip %+v %v", result, err)
	}
	expected, err := s.Stat(ctx, "target")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(filepath.Join(root, "target"), filepath.Join(root, "previous")); err != nil {
		t.Fatal(err)
	}
	writeLocal(t, s, "target", []byte("replaced"))
	if reader, _, err := s.Open(ctx, "target", OpenOptions{Expected: &expected}); !errors.Is(err, ErrSourceChanged) {
		if reader != nil {
			reader.Close()
		}
		t.Fatalf("stale source accepted %v", err)
	}
	reader, _, err := s.Open(ctx, "target", OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if err = os.WriteFile(filepath.Join(root, "target"), []byte("replacement changed length"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = io.ReadAll(reader); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("in-flight change accepted %v", err)
	}
	var wg sync.WaitGroup
	results := make(chan WriteResult, 2)
	errs := make(chan error, 2)
	for _, text := range []string{"one", "two"} {
		wg.Add(1)
		go func(text string) {
			defer wg.Done()
			r, e := s.Write(ctx, "race", strings.NewReader(text), WriteOptions{Size: 3})
			results <- r
			errs <- e
		}(text)
	}
	wg.Wait()
	committed, skipped := 0, 0
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		r := <-results
		if r.Committed {
			committed++
		}
		if r.Skipped {
			skipped++
		}
	}
	if committed != 1 || skipped != 1 {
		t.Fatalf("concurrent SKIP not atomic: committed%d skipped%d", committed, skipped)
	}
}

func TestLocalEmptyDirectoryAndCanceledOperations(t *testing.T) {
	s, _ := localFixture(t)
	ctx := context.Background()
	writeLocal(t, s, "empty", nil)
	r, err := s.Write(ctx, "directory/", strings.NewReader(""), WriteOptions{Size: 0})
	if err != nil || !r.Committed {
		t.Fatalf("directory %+v %v", r, err)
	}
	if object, err := s.Stat(ctx, "directory/"); err != nil || object.Size != 0 {
		t.Fatalf("marker %+v %v", object, err)
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	for name, operation := range map[string]func() error{"list": func() error { _, e := s.List(ctx, ListOptions{}); return e }, "scan": func() error { return s.Scan(ctx, ListOptions{}, func(Object) error { return nil }) }, "stat": func() error { _, e := s.Stat(ctx, "empty"); return e }, "open": func() error { _, _, e := s.Open(ctx, "empty", OpenOptions{}); return e }, "write": func() error { _, e := s.Write(ctx, "empty", strings.NewReader(""), WriteOptions{}); return e }} {
		if err := operation(); !errors.Is(err, context.Canceled) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, limit := range []int{-1, 1001} {
		if _, err := s.List(context.Background(), ListOptions{Limit: limit}); err == nil {
			t.Error(fmt.Sprintf("accepted limit %d", limit))
		}
	}
}
