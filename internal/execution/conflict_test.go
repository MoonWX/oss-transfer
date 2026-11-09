package execution

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func TestValidConflictPolicy(t *testing.T) {
	for _, policy := range []string{"SKIP", "OVERWRITE", "OVERWRITE_CHANGED"} {
		if !ValidConflictPolicy(policy) {
			t.Fatalf("%s rejected", policy)
		}
	}
	for _, policy := range []string{"", "skip", "CHANGED", "OVERWRITE_NEWER"} {
		if ValidConflictPolicy(policy) {
			t.Fatalf("%q accepted", policy)
		}
	}
}

func TestDestinationOutdatedRule(t *testing.T) {
	early := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	late := early.Add(time.Second)
	const md5A, md5B = "0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210"
	object := func(size int64, etag string, modified time.Time) transferstore.Object {
		return transferstore.Object{Key: "k", Size: size, ETag: etag, ModifiedAt: modified}
	}
	cases := []struct {
		name        string
		source, dst transferstore.Object
		want        bool
	}{
		{"sizes differ", object(2, md5A, early), object(3, md5A, late), true},
		{"sizes differ without metadata", object(2, "", time.Time{}), object(3, "", time.Time{}), true},
		{"md5 differ", object(3, `"`+md5A+`"`, early), object(3, md5B, late), true},
		{"md5 equal ignoring quotes and case", object(3, `"`+md5A+`"`, early), object(3, "0123456789ABCDEF0123456789ABCDEF", late), false},
		{"md5 equal but source newer", object(3, md5A, late), object(3, md5A, early), true},
		{"multipart etags differ", object(3, md5A+"-2", early), object(3, md5B+"-3", late), false},
		{"multipart against md5", object(3, md5A+"-2", early), object(3, md5B, late), false},
		{"md5 against multipart", object(3, md5A, early), object(3, md5B+"-2", late), false},
		{"multipart and source newer", object(3, md5A+"-2", late), object(3, md5B+"-2", early), true},
		{"local fingerprints", object(3, "local:1:2:3", early), object(3, "local:4:5:6", late), false},
		{"weak etags never compared", object(3, `W/"`+md5A+`"`, early), object(3, `W/"`+md5B+`"`, late), false},
		{"non-hex 32 characters", object(3, "zz23456789abcdef0123456789abcdef", early), object(3, md5B, late), false},
		{"source newer", object(3, "", late), object(3, "", early), true},
		{"same time", object(3, "", early), object(3, "", early), false},
		{"destination newer", object(3, "", early), object(3, "", late), false},
		{"source time unknown", object(3, "", time.Time{}), object(3, "", early), false},
		{"destination time unknown", object(3, "", late), object(3, "", time.Time{}), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DestinationOutdated(tc.source, tc.dst); got != tc.want {
				t.Fatalf("DestinationOutdated = %v, want %v", got, tc.want)
			}
		})
	}
}

// memoryStore is a minimal object store whose ETags are single-part MD5
// digests unless a test sets one. It records every Open and Write.
type memoryStore struct {
	mu      sync.Mutex
	objects map[string]memoryObject
	opens   map[string]int
	writes  map[string]transferstore.WriteOptions
}

type memoryObject struct {
	data []byte
	meta transferstore.Object
}

func newMemoryStore() *memoryStore {
	return &memoryStore{objects: map[string]memoryObject{}, opens: map[string]int{}, writes: map[string]transferstore.WriteOptions{}}
}

func (m *memoryStore) put(key string, data []byte, etag string, modified time.Time) transferstore.Object {
	if etag == "" {
		sum := md5.Sum(data)
		etag = `"` + hex.EncodeToString(sum[:]) + `"`
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	meta := transferstore.Object{Key: key, Size: int64(len(data)), ETag: etag, ModifiedAt: modified}
	m.objects[key] = memoryObject{data: append([]byte(nil), data...), meta: meta}
	return meta
}

func (m *memoryStore) data(key string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	object, ok := m.objects[key]
	return object.data, ok
}

func (m *memoryStore) List(context.Context, transferstore.ListOptions) (transferstore.Page, error) {
	return transferstore.Page{}, transferstore.ErrUnsupported
}
func (m *memoryStore) Scan(context.Context, transferstore.ListOptions, func(transferstore.Object) error) error {
	return transferstore.ErrUnsupported
}
func (m *memoryStore) Stat(_ context.Context, key string) (transferstore.Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	object, ok := m.objects[key]
	if !ok {
		return transferstore.Object{}, transferstore.ErrNotFound
	}
	return object.meta, nil
}
func (m *memoryStore) Open(_ context.Context, key string, options transferstore.OpenOptions) (io.ReadCloser, transferstore.Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.opens[key]++
	object, ok := m.objects[key]
	if !ok {
		return nil, transferstore.Object{}, transferstore.ErrNotFound
	}
	if expected := options.Expected; expected != nil && (expected.Size != object.meta.Size || !transferstore.EqualETag(expected.ETag, object.meta.ETag)) {
		return nil, transferstore.Object{}, transferstore.ErrSourceChanged
	}
	return io.NopCloser(bytes.NewReader(object.data)), object.meta, nil
}
func (m *memoryStore) Write(_ context.Context, key string, input io.Reader, options transferstore.WriteOptions) (transferstore.WriteResult, error) {
	data, err := io.ReadAll(input)
	if err != nil {
		return transferstore.WriteResult{}, err
	}
	m.mu.Lock()
	m.writes[key] = options
	prior, exists := m.objects[key]
	m.mu.Unlock()
	if exists && !options.Overwrite {
		return transferstore.WriteResult{Object: prior.meta, Skipped: true}, nil
	}
	if int64(len(data)) != options.Size {
		return transferstore.WriteResult{}, transferstore.ErrSizeMismatch
	}
	object := m.put(key, data, "", time.Now())
	return transferstore.WriteResult{Object: object, Committed: true}, nil
}
func (m *memoryStore) Close() error { return nil }

// Every branch of OVERWRITE_CHANGED through the engine, with S3-like MD5
// ETags: only outdated destinations are replaced, and a skipped object's
// content is never read.
func TestOverwriteChangedEngineDecisions(t *testing.T) {
	f := newFixture(t, 0)
	f.job.ConflictPolicy = ConflictOverwriteChanged
	source, target := newMemoryStore(), newMemoryStore()
	t0 := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	type entry struct {
		key        string
		source     []byte
		sourceETag string
		sourceTime time.Time
		dest       []byte // nil: missing at the destination
		destETag   string
		destTime   time.Time
		copied     bool
	}
	entries := []entry{
		{key: "missing", source: []byte("new"), sourceTime: t0, copied: true},
		{key: "size", source: []byte("longer"), sourceTime: t0, dest: []byte("short"), destTime: t0.Add(time.Hour), copied: true},
		{key: "md5", source: []byte("abc"), sourceTime: t0, dest: []byte("xyz"), destTime: t0.Add(time.Hour), copied: true},
		{key: "same", source: []byte("abc"), sourceTime: t0, dest: []byte("abc"), destTime: t0.Add(time.Hour)},
		{key: "same-but-newer", source: []byte("abc"), sourceTime: t0.Add(2 * time.Hour), dest: []byte("abc"), destTime: t0.Add(time.Hour), copied: true},
		{key: "multipart", source: []byte("abc"), sourceETag: `"0123456789abcdef0123456789abcdef-2"`, sourceTime: t0, dest: []byte("xyz"), destETag: `"fedcba9876543210fedcba9876543210-3"`, destTime: t0.Add(time.Hour)},
		{key: "multipart-newer", source: []byte("abc"), sourceETag: `"0123456789abcdef0123456789abcdef-2"`, sourceTime: t0.Add(2 * time.Hour), dest: []byte("xyz"), destETag: `"fedcba9876543210fedcba9876543210-3"`, destTime: t0.Add(time.Hour), copied: true},
	}
	for i, e := range entries {
		object := source.put(e.key, e.source, e.sourceETag, e.sourceTime)
		if e.dest != nil {
			target.put("copied/"+e.key, e.dest, e.destETag, e.destTime)
		}
		f.job.Items = append(f.job.Items, Item{ID: fmt.Sprint(i + 1), Source: object, DestinationKey: "copied/" + e.key})
	}
	f.startEngine(StoreFactoryFunc(func(context.Context, Job) (transferstore.Store, transferstore.Store, error) {
		return source, target, nil
	}), Options{})
	f.acceptStart()
	r := f.wait()
	copied := 0
	for _, e := range entries {
		got, _ := target.data("copied/" + e.key)
		source.mu.Lock()
		opens := source.opens[e.key]
		source.mu.Unlock()
		target.mu.Lock()
		options, wrote := target.writes["copied/"+e.key]
		target.mu.Unlock()
		if e.copied {
			copied++
			if !bytes.Equal(got, e.source) || opens != 1 || !wrote {
				t.Errorf("%s: want copied, got %q opens=%d wrote=%v", e.key, got, opens, wrote)
			}
			if e.dest != nil && !options.Overwrite {
				t.Errorf("%s: an outdated destination was written without overwrite", e.key)
			}
			if e.dest == nil && (options.Overwrite || !options.DestinationAbsent) {
				t.Errorf("%s: a missing destination must be written conditionally: %+v", e.key, options)
			}
		} else if !bytes.Equal(got, e.dest) || opens != 0 || wrote {
			t.Errorf("%s: want skipped without reading the source, got %q opens=%d wrote=%v", e.key, got, opens, wrote)
		}
	}
	if r.Status != Completed || r.Counters.Succeeded != int64(copied) || r.Counters.Skipped != int64(len(entries)-copied) || r.Counters.Failed != 0 {
		t.Fatalf("unexpected result: %s %+v", r.Status, r.Counters)
	}
}

// OVERWRITE_CHANGED with local files on both sides: local fingerprints are
// never compared as ETags, so size and modification time decide.
func TestOverwriteChangedLocalFiles(t *testing.T) {
	f := newFixture(t, 4)
	f.job.ConflictPolicy = ConflictOverwriteChanged
	write := func(item Item, data []byte, modified time.Time) {
		name := filepath.Join(f.dst, filepath.FromSlash(item.DestinationKey))
		if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(name, modified, modified); err != nil {
			t.Fatal(err)
		}
	}
	items := f.job.Items
	// Item 0 is missing at the destination and is copied.
	// Item 1: a different size is replaced.
	write(items[1], []byte("different size"), items[1].Source.ModifiedAt.Add(time.Hour))
	// Item 2: same size, older than the source: replaced.
	stale := bytes.Repeat([]byte{0xee}, int(items[2].Source.Size))
	write(items[2], stale, items[2].Source.ModifiedAt.Add(-time.Hour))
	// Item 3: same size and newer than the source: kept even though its
	// content differs, because content is never read.
	kept := bytes.Repeat([]byte{0xdd}, int(items[3].Source.Size))
	write(items[3], kept, items[3].Source.ModifiedAt.Add(time.Hour))
	f.startEngine(nil, Options{})
	f.acceptStart()
	r := f.wait()
	if r.Status != Completed || r.Counters.Succeeded != 3 || r.Counters.Skipped != 1 || r.Counters.Failed != 0 {
		t.Fatalf("unexpected result: %s %+v", r.Status, r.Counters)
	}
	for i, item := range items {
		got, err := os.ReadFile(filepath.Join(f.dst, filepath.FromSlash(item.DestinationKey)))
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(filepath.Join(f.src, item.Source.Key))
		if err != nil {
			t.Fatal(err)
		}
		if i == 3 {
			want = kept
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("item %d: unexpected destination content", i)
		}
	}
}
