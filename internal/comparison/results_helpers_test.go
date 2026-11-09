package comparison

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// fakeStore is a sorted, read-only listing of one side. Objects carry an ETag
// that is not an MD5 digest, so metadata comparisons depend only on size and
// headers with both the sequential and the concurrent engine.
type fakeStore struct {
	transferstore.Store
	objects []transferstore.Object
	// listHook runs before every LIST; an error fails it.
	listHook func(context.Context, transferstore.ListOptions) error
	closeErr error
	closed   atomic.Int32
}

func object(key string, size int64) transferstore.Object {
	return transferstore.Object{Key: key, Size: size, ETag: `"fixture"`}
}

func newFakeStore(objects ...transferstore.Object) *fakeStore {
	sorted := append([]transferstore.Object(nil), objects...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Key < sorted[j].Key })
	return &fakeStore{objects: sorted}
}

func (s *fakeStore) List(ctx context.Context, o transferstore.ListOptions) (transferstore.Page, error) {
	if s.listHook != nil {
		if err := s.listHook(ctx, o); err != nil {
			return transferstore.Page{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return transferstore.Page{}, err
	}
	after := max(o.StartAfter, o.Cursor)
	start := sort.Search(len(s.objects), func(i int) bool { k := s.objects[i].Key; return k > after && k >= o.Prefix })
	limit := o.Limit
	if limit <= 0 {
		limit = 1000
	}
	page := transferstore.Page{}
	end := start
	for end < len(s.objects) && len(page.Objects) < limit && strings.HasPrefix(s.objects[end].Key, o.Prefix) {
		page.Objects = append(page.Objects, s.objects[end])
		end++
	}
	if end < len(s.objects) && strings.HasPrefix(s.objects[end].Key, o.Prefix) {
		page.NextCursor = s.objects[end-1].Key
	}
	return page, nil
}

func (s *fakeStore) Stat(ctx context.Context, key string) (transferstore.Object, error) {
	if err := ctx.Err(); err != nil {
		return transferstore.Object{}, err
	}
	i := sort.Search(len(s.objects), func(i int) bool { return s.objects[i].Key >= key })
	if i < len(s.objects) && s.objects[i].Key == key {
		return s.objects[i], nil
	}
	return transferstore.Object{}, transferstore.ErrNotFound
}

func (s *fakeStore) Close() error {
	s.closed.Add(1)
	return s.closeErr
}

// blockAfter makes LIST wait for cancellation once a listing reaches key.
func blockAfter(key string) func(context.Context, transferstore.ListOptions) error {
	return func(ctx context.Context, o transferstore.ListOptions) error {
		if max(o.Cursor, o.StartAfter) >= key {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
}

func storesFactory(source, dest transferstore.Store) execution.StoreFactory {
	return execution.StoreFactoryFunc(func(context.Context, execution.Job) (transferstore.Store, transferstore.Store, error) {
		return source, dest, nil
	})
}

// testLog collects Logf lines.
type testLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *testLog) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *testLog) contains(text string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		if strings.Contains(line, text) {
			return true
		}
	}
	return false
}

// testClock is a manual clock for Options.Now.
type testClock struct{ now atomic.Int64 }

func newTestClock() *testClock {
	c := &testClock{}
	c.now.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	return c
}

func (c *testClock) Now() time.Time          { return time.Unix(0, c.now.Load()).UTC() }
func (c *testClock) advance(d time.Duration) { c.now.Add(int64(d)) }

// harness owns a database and a results directory that outlive Managers.
type harness struct {
	t   *testing.T
	db  *state.Store
	dir string
	log *testLog
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &harness{t: t, db: db, dir: filepath.Join(t.TempDir(), "comparisons"), log: &testLog{}}
}

func (h *harness) open(factory execution.StoreFactory, configure ...func(*Options)) *Manager {
	h.t.Helper()
	// Tests do not depend on the free space of the machine running them.
	options := Options{Directory: h.dir, Logf: h.log.logf, FreeBytes: func(string) (uint64, error) { return 1 << 50, nil }}
	for _, f := range configure {
		f(&options)
	}
	m, err := New(h.db, factory, nil, options)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(m.Close)
	return m
}

// record reads the committed record, including internal fields.
func (h *harness) record(id string) runRecord {
	h.t.Helper()
	var rec runRecord
	if err := h.db.View(func(tx *state.Tx) error {
		found, err := tx.Get(runsBucket, id, &rec)
		if err == nil && !found {
			err = fmt.Errorf("record %s missing", id)
		}
		return err
	}); err != nil {
		h.t.Fatal(err)
	}
	return rec
}

func (h *harness) put(bucket, key string, value any) {
	h.t.Helper()
	if err := h.db.Update(func(tx *state.Tx) error { return tx.Put(bucket, key, value) }); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) count(bucket, prefix string) int {
	h.t.Helper()
	n := 0
	if err := h.db.View(func(tx *state.Tx) error {
		return tx.ScanKeys(bucket, prefix, "", 1<<30, func(string, int64) error { n++; return nil })
	}); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func (h *harness) exists(bucket, key string) bool {
	h.t.Helper()
	found := false
	if err := h.db.View(func(tx *state.Tx) error {
		var raw json.RawMessage
		var err error
		found, err = tx.Get(bucket, key, &raw)
		return err
	}); err != nil {
		h.t.Fatal(err)
	}
	return found
}

func (h *harness) resultsPath(id string) string {
	return filepath.Join(h.dir, resultDirectory(id), resultsFile)
}

func (h *harness) fileSize(id string) int64 {
	h.t.Helper()
	info, err := os.Stat(h.resultsPath(id))
	if err != nil {
		h.t.Fatal(err)
	}
	return info.Size()
}

// accessDenied is the error the S3 SDK returns for HTTP 403 AccessDenied: a
// failure no engine retries.
func accessDenied() error {
	return &smithy.OperationError{ServiceID: "S3", OperationName: "ListObjectsV2", Err: &awshttp.ResponseError{
		ResponseError: &smithyhttp.ResponseError{Response: &smithyhttp.Response{Response: &http.Response{StatusCode: http.StatusForbidden}}, Err: &smithy.GenericAPIError{Code: "AccessDenied", Message: "Access Denied"}},
		RequestID:     "FIXTURE",
	}}
}

func requestWithID(id string) Request {
	r := request()
	r.ID = id
	return r
}

// startAndWait runs a comparison to its terminal state.
func startAndWait(t *testing.T, m *Manager, r Request, factories ...execution.StoreFactory) Progress {
	t.Helper()
	if _, err := m.Start(r, factories...); err != nil {
		t.Fatal(err)
	}
	waitWorkers(t, m)
	p, err := m.Get(r.ID, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	return p.Progress
}

// waitFor polls cond; the deadline is only a hang guard.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// allRows pages through every stored row with q (ID, Limit, Statuses and
// MaxBytes are kept) and checks the cursor contract on every page.
func allRows(t *testing.T, m *Manager, q RowsQuery) []Row {
	t.Helper()
	var rows []Row
	q.After = 0
	for pages := 0; ; pages++ {
		if pages > 1_000_000 {
			t.Fatal("paging did not end")
		}
		page, err := m.Rows(q)
		if err != nil {
			t.Fatal(err)
		}
		previous := q.After
		for _, row := range page.Items {
			if row.Number <= previous || row.Number > page.Rows {
				t.Fatalf("row %d outside page window after %d of %d rows", row.Number, previous, page.Rows)
			}
			previous = row.Number
		}
		rows = append(rows, page.Items...)
		if page.NextAfter == 0 {
			return rows
		}
		if page.NextAfter <= q.After || len(page.Items) > 0 && page.NextAfter < page.Items[len(page.Items)-1].Number {
			t.Fatalf("cursor did not advance: after %d next %d", q.After, page.NextAfter)
		}
		q.After = page.NextAfter
	}
}

func encodedSize(t *testing.T, rows []Row) int {
	t.Helper()
	size := 0
	for _, row := range rows {
		data, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		size += len(data) + 1
	}
	return size
}
