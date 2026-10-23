package comparison

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

type inspectedStore struct {
	transferstore.Store
	opens  atomic.Int64
	stats  atomic.Int64
	mutate func()
	etag   string
}

type generatedStore struct {
	transferstore.Store
	count int
}

func (s generatedStore) List(_ context.Context, options transferstore.ListOptions) (transferstore.Page, error) {
	start, _ := strconv.Atoi(options.Cursor)
	end := min(start+options.Limit, s.count)
	p := transferstore.Page{}
	for i := start; i < end; i++ {
		p.Objects = append(p.Objects, transferstore.Object{Key: fmt.Sprintf("src/%08d", i), Size: 1, ETag: "stable"})
	}
	if end < s.count {
		p.NextCursor = strconv.Itoa(end)
	}
	return p, nil
}
func (s generatedStore) Stat(_ context.Context, key string) (transferstore.Object, error) {
	i, e := strconv.Atoi(strings.TrimPrefix(key, "src/"))
	if e != nil || !strings.HasPrefix(key, "src/") || i < 0 || i >= s.count {
		return transferstore.Object{}, transferstore.ErrNotFound
	}
	return transferstore.Object{Key: key, Size: 1, ETag: "stable"}, nil
}
func (s generatedStore) Close() error { return nil }

func TestManagerCapsStoredDifferencesKeepsFullCountsAndPrunesOldRuns(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	count := MaxStoredDifferences + 3
	m, err := New(db, execution.StoreFactoryFunc(func(context.Context, execution.Job) (transferstore.Store, transferstore.Store, error) {
		return generatedStore{count: count}, generatedStore{}, nil
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	finish := func(id string) Page {
		t.Helper()
		r := request()
		r.ID = id
		deadline := time.Now().Add(5 * time.Second)
		for {
			_, err = m.Start(r)
			if err == nil {
				break
			}
			if !errors.Is(err, ErrBusy) || time.Now().After(deadline) {
				t.Fatal(err)
			}
			time.Sleep(time.Millisecond)
		}
		for {
			p, e := m.Get(id, 0, 50)
			if e != nil {
				t.Fatal(e)
			}
			if p.Status == "COMPLETED" {
				return p
			}
			if time.Now().After(deadline) {
				t.Fatalf("comparison stuck: %+v", p)
			}
			time.Sleep(time.Millisecond)
		}
	}
	p := finish("oldest")
	if p.Rows != MaxStoredDifferences || !p.RowsTruncated || p.Missing != int64(count) || p.Compared != int64(count) {
		t.Fatalf("cap lost totals: %+v", p.Progress)
	}
	count = 0
	for i := 0; i < RetainedComparisons; i++ {
		finish(fmt.Sprintf("later-%02d", i))
	}
	if _, err = m.Get("oldest", 0, 50); !errors.Is(err, ErrNotFound) {
		t.Fatalf("oldest result not pruned: %v", err)
	}
	var leftover int
	_ = db.View(func(tx *state.Tx) error {
		return tx.Scan("comparison_rows", "oldest/", "", 1, func(string, json.RawMessage) error { leftover++; return nil })
	})
	if leftover != 0 {
		t.Fatal("pruning left orphaned difference rows")
	}
}

func (s *inspectedStore) Stat(ctx context.Context, key string) (transferstore.Object, error) {
	s.stats.Add(1)
	o, e := s.Store.Stat(ctx, key)
	if s.etag != "" {
		o.ETag = s.etag
	}
	return o, e
}
func (s *inspectedStore) List(ctx context.Context, o transferstore.ListOptions) (transferstore.Page, error) {
	p, e := s.Store.List(ctx, o)
	for i := range p.Objects {
		if s.etag != "" {
			p.Objects[i].ETag = s.etag
		}
	}
	return p, e
}
func (s *inspectedStore) Open(ctx context.Context, key string, o transferstore.OpenOptions) (io.ReadCloser, transferstore.Object, error) {
	s.opens.Add(1)
	r, a, e := s.Store.Open(ctx, key, o)
	if e == nil && s.mutate != nil {
		s.mutate()
	}
	return r, a, e
}
func local(t *testing.T) *inspectedStore {
	t.Helper()
	s, e := transferstore.NewLocal(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return &inspectedStore{Store: s}
}
func put(t *testing.T, s transferstore.Store, key, value string) {
	t.Helper()
	_, e := s.Write(context.Background(), key, bytes.NewBufferString(value), transferstore.WriteOptions{Size: int64(len(value)), Overwrite: true})
	if e != nil {
		t.Fatal(e)
	}
}
func request() Request {
	return Request{ID: "test", Source: execution.Endpoint{Kind: "S3", Ref: "src", Bucket: "src"}, Destination: execution.Endpoint{Kind: "S3", Ref: "dst", Bucket: "dst"}, Scope: "prefix", Mode: "metadata", SourcePrefix: "src/", DestinationPrefix: "backup/"}
}
func collect(t *testing.T, r Request, a, b transferstore.Store) []Row {
	t.Helper()
	var rows []Row
	err := Run(context.Background(), r, a, b, nil, func(row Row) error { rows = append(rows, row); return nil })
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestMetadataDoesNotReadContentOrTreatETagAsHash(t *testing.T) {
	a, b := local(t), local(t)
	put(t, a, "src/a", "AAAA")
	put(t, b, "backup/a", "BBBB")
	a.etag = "multipart-3"
	b.etag = "opaque-encrypted"
	rows := collect(t, request(), a, b)
	if len(rows) != 1 || rows[0].Status != "MATCH" || a.opens.Load() != 0 || b.opens.Load() != 0 {
		t.Fatalf("metadata claimed content equality or read bodies: %+v", rows)
	}
}

func TestStreamingDetectsSameSizeDifferenceAndBothSideMissing(t *testing.T) {
	a, b := local(t), local(t)
	put(t, a, "src/a", "same")
	put(t, b, "backup/a", "same")
	put(t, a, "src/b", "AAAA")
	put(t, b, "backup/b", "BBBB")
	put(t, a, "src/missing", "data")
	put(t, b, "backup/extra", "data")
	r := request()
	r.Mode = "content"
	rows := collect(t, r, a, b)
	want := map[string]string{"a": "MATCH", "b": "DIFFERENT", "missing": "MISSING_TARGET", "extra": "EXTRA_TARGET"}
	for _, row := range rows {
		if want[row.Key] != row.Status {
			t.Fatalf("wrong result: %+v", row)
		}
		delete(want, row.Key)
		if row.Key == "b" && (row.SourceSHA256 == row.DestinationSHA256 || len(row.SourceSHA256) != 64) {
			t.Fatalf("missing real SHA-256: %+v", row)
		}
	}
	if len(want) != 0 || a.opens.Load() != 2 || b.opens.Load() != 2 {
		t.Fatalf("missing results or unexpected reads: %v %d %d", want, a.opens.Load(), b.opens.Load())
	}
}

func TestMergePaginatesBothSidesAndPreservesExactKeyMapping(t *testing.T) {
	a, b := local(t), local(t)
	for i := 0; i < 520; i++ {
		k := fmt.Sprintf("%04d", i)
		put(t, a, "src/"+k, "x")
		put(t, b, "backup/"+k, "x")
	}
	put(t, b, "backup/zz extra + 字", "x")
	rows := collect(t, request(), a, b)
	if len(rows) != 521 || rows[520].Status != "EXTRA_TARGET" || rows[520].SourceKey != "src/zz extra + 字" {
		t.Fatalf("lost paged data: %d %+v", len(rows), rows[len(rows)-1])
	}
}

func TestContentChangedWhileOtherSideStreamsIsNotAMatch(t *testing.T) {
	a, b := local(t), local(t)
	put(t, a, "src/a", "AAAA")
	put(t, b, "backup/a", "AAAA")
	b.mutate = func() { put(t, a, "src/a", "BBBB") }
	r := request()
	r.Mode = "content"
	rows := collect(t, r, a, b)
	if rows[0].Status != "CHANGED_DURING_COMPARE" {
		t.Fatalf("changing object accepted: %+v", rows)
	}
}

type blockedStore struct{ transferstore.Store }

func (s blockedStore) List(ctx context.Context, _ transferstore.ListOptions) (transferstore.Page, error) {
	<-ctx.Done()
	return transferstore.Page{}, ctx.Err()
}
func TestManagerCancellationPersistenceAndSingleActiveBound(t *testing.T) {
	db, e := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	a, b := local(t), local(t)
	factory := execution.StoreFactoryFunc(func(context.Context, execution.Job) (transferstore.Store, transferstore.Store, error) {
		return blockedStore{a}, b, nil
	})
	m, e := New(db, factory, nil)
	if e != nil {
		t.Fatal(e)
	}
	r := request()
	if _, e = m.Start(r); e != nil {
		t.Fatal(e)
	}
	r.ID = "second"
	if _, e = m.Start(r); !errors.Is(e, ErrBusy) {
		t.Fatalf("unbounded active comparisons: %v", e)
	}
	if _, e = m.Cancel("test"); e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(time.Second)
	for {
		p, e := m.Get("test", 0, 50)
		if e != nil {
			t.Fatal(e)
		}
		if p.Status == "CANCELED" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cancel did not finish: %+v", p)
		}
		time.Sleep(time.Millisecond)
	}
	m.Close()
	m, e = New(db, factory, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer m.Close()
	p, e := m.Get("test", 0, 50)
	if e != nil || p.Status != "CANCELED" {
		t.Fatalf("state lost after restart: %+v %v", p, e)
	}
}

func TestManagerBatchesOnlyDifferencesAndPagesResults(t *testing.T) {
	db, e := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	a, b := local(t), local(t)
	for i := 0; i < 140; i++ {
		put(t, a, fmt.Sprintf("src/%04d", i), "x")
	}
	put(t, a, "src/match", "same")
	put(t, b, "backup/match", "same")
	m, e := New(db, execution.StoreFactoryFunc(func(context.Context, execution.Job) (transferstore.Store, transferstore.Store, error) {
		return a, b, nil
	}), nil)
	if e != nil {
		t.Fatal(e)
	}
	defer m.Close()
	if _, e = m.Start(request()); e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(3 * time.Second)
	var p Page
	for {
		p, e = m.Get("test", 0, 100)
		if e != nil {
			t.Fatal(e)
		}
		if p.Status == "COMPLETED" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("comparison stuck: %+v", p)
		}
		time.Sleep(time.Millisecond)
	}
	if p.Compared != 141 || p.Matched != 1 || p.Missing != 140 || p.Rows != 140 || len(p.Items) != 100 || p.NextAfter != 100 {
		t.Fatalf("wrong totals/pagination: %+v", p.Progress)
	}
	p, e = m.Get("test", 100, 100)
	if e != nil || len(p.Items) != 40 || p.NextAfter != 0 || p.Items[0].Number != 101 {
		t.Fatalf("lost persisted rows: %+v %v", p, e)
	}
}
