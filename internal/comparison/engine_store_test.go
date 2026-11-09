package comparison

// Helpers for the engine tests. Every identifier starts with "engine" so these
// files merge cleanly with other test files of the package.

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// engineHangGuard bounds waits that only fail when the code under test hangs.
const engineHangGuard = 2 * time.Minute

// engineObject is one object of an engineStore.
type engineObject struct {
	data        []byte
	etag        string // "" means no ETag
	version     string
	modified    time.Time
	contentType string
	metadata    map[string]string
}

// engineStore is an in-memory transferstore.Store. Hooks are set before Run
// and only read while it runs.
type engineStore struct {
	mu       sync.Mutex
	objects  map[string]engineObject
	attempts map[string]int
	listLog  []transferstore.ListOptions
	rangeLog []string // "key@offset+length"

	// listHook replaces List when it returns handled.
	listHook func(ctx context.Context, options transferstore.ListOptions, call int) (page transferstore.Page, handled bool, err error)
	// statHook runs before every Stat; an error is returned as the result.
	statHook func(ctx context.Context, key string) error
	// openHook replaces Open's body when it returns handled. attempt counts
	// Open calls for the key from 1.
	openHook func(ctx context.Context, key string, data []byte, attempt int) (body io.ReadCloser, handled bool, err error)
	// rangeHook replaces OpenRange's body when it returns handled. attempt
	// counts OpenRange calls for the key and offset from 1.
	rangeHook func(ctx context.Context, key string, data []byte, offset, length int64, attempt int) (body io.ReadCloser, handled bool, err error)

	lists, stats, opens atomic.Int64
}

func newEngineStore() *engineStore {
	return &engineStore{objects: map[string]engineObject{}, attempts: map[string]int{}}
}

// put stores data with a simple MD5 ETag and a fixed modification time.
func (s *engineStore) put(key, data string) {
	s.putObject(key, engineObject{data: []byte(data), etag: engineMD5ETag([]byte(data)), modified: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
}

func (s *engineStore) putObject(key string, object engineObject) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = object
}

func (s *engineStore) remove(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, key)
}

func engineMD5ETag(data []byte) string {
	sum := md5.Sum(data)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

func (s *engineStore) attempt(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts[name]++
	return s.attempts[name]
}

func (s *engineStore) describe(key string, object engineObject) transferstore.Object {
	return transferstore.Object{Key: key, Size: int64(len(object.data)), ETag: object.etag, VersionID: object.version, ModifiedAt: object.modified, ContentType: object.contentType, Metadata: object.metadata}
}

func (s *engineStore) List(ctx context.Context, options transferstore.ListOptions) (transferstore.Page, error) {
	call := int(s.lists.Add(1))
	s.mu.Lock()
	s.listLog = append(s.listLog, options)
	s.mu.Unlock()
	if s.listHook != nil {
		if page, handled, err := s.listHook(ctx, options, call); handled {
			return page, err
		}
	}
	if err := ctx.Err(); err != nil {
		return transferstore.Page{}, err
	}
	limit := options.Limit
	if limit == 0 {
		limit = 1000
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.objects))
	for key := range s.objects {
		if strings.HasPrefix(key, options.Prefix) && key > options.StartAfter && key > options.Cursor {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	page := transferstore.Page{Objects: []transferstore.Object{}}
	for _, key := range keys {
		if len(page.Objects) == limit {
			page.NextCursor = page.Objects[limit-1].Key
			break
		}
		page.Objects = append(page.Objects, s.describe(key, s.objects[key]))
	}
	return page, nil
}

func (s *engineStore) Scan(ctx context.Context, options transferstore.ListOptions, emit func(transferstore.Object) error) error {
	for {
		page, err := s.List(ctx, options)
		if err != nil {
			return err
		}
		for _, object := range page.Objects {
			if err := emit(object); err != nil {
				return err
			}
		}
		if page.NextCursor == "" {
			return nil
		}
		options.Cursor = page.NextCursor
	}
}

func (s *engineStore) Stat(ctx context.Context, key string) (transferstore.Object, error) {
	s.stats.Add(1)
	if s.statHook != nil {
		if err := s.statHook(ctx, key); err != nil {
			return transferstore.Object{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return transferstore.Object{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	object, ok := s.objects[key]
	if !ok {
		return transferstore.Object{}, fmt.Errorf("%w: %s", transferstore.ErrNotFound, key)
	}
	return s.describe(key, object), nil
}

// pinned returns the current object when it still matches expected.
func (s *engineStore) pinned(key string, expected *transferstore.Object) (transferstore.Object, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	object, ok := s.objects[key]
	if !ok {
		return transferstore.Object{}, nil, transferstore.ErrNotFound
	}
	described := s.describe(key, object)
	if expected != nil && (expected.Size != described.Size || expected.ETag != "" && !transferstore.EqualETag(expected.ETag, described.ETag)) {
		return transferstore.Object{}, nil, transferstore.ErrSourceChanged
	}
	return described, object.data, nil
}

func (s *engineStore) Open(ctx context.Context, key string, options transferstore.OpenOptions) (io.ReadCloser, transferstore.Object, error) {
	s.opens.Add(1)
	if err := ctx.Err(); err != nil {
		return nil, transferstore.Object{}, err
	}
	object, data, err := s.pinned(key, options.Expected)
	if err != nil {
		return nil, transferstore.Object{}, err
	}
	if s.openHook != nil {
		if body, handled, err := s.openHook(ctx, key, data, s.attempt("open "+key)); handled {
			return body, object, err
		}
	}
	return engineBody(ctx, data), object, nil
}

func (s *engineStore) Write(ctx context.Context, key string, input io.Reader, options transferstore.WriteOptions) (transferstore.WriteResult, error) {
	data, err := io.ReadAll(input)
	if err != nil {
		return transferstore.WriteResult{}, err
	}
	s.putObject(key, engineObject{data: data, etag: engineMD5ETag(data), modified: time.Now().UTC()})
	return transferstore.WriteResult{Committed: true}, nil
}

func (s *engineStore) Close() error { return nil }

// engineRangeStore adds ranged reads to an engineStore.
type engineRangeStore struct {
	*engineStore
	unsupported bool
}

func (s *engineRangeStore) OpenRanges(ctx context.Context, key string, options transferstore.OpenOptions) (transferstore.RangeSource, transferstore.Object, error) {
	if s.unsupported {
		return nil, transferstore.Object{}, fmt.Errorf("%w: test store serves no ranges", transferstore.ErrUnsupported)
	}
	if err := ctx.Err(); err != nil {
		return nil, transferstore.Object{}, err
	}
	object, data, err := s.pinned(key, options.Expected)
	if err != nil {
		return nil, transferstore.Object{}, err
	}
	return &engineRanges{store: s.engineStore, key: key, data: data}, object, nil
}

type engineRanges struct {
	store  *engineStore
	key    string
	data   []byte
	closed atomic.Bool
}

func (r *engineRanges) OpenRange(ctx context.Context, offset, length int64) (io.ReadCloser, error) {
	if offset < 0 || length <= 0 || offset+length > int64(len(r.data)) {
		return nil, errors.New("source range is outside the object")
	}
	r.store.mu.Lock()
	r.store.rangeLog = append(r.store.rangeLog, fmt.Sprintf("%s@%d+%d", r.key, offset, length))
	r.store.mu.Unlock()
	attempt := r.store.attempt(fmt.Sprintf("range %s@%d", r.key, offset))
	if hook := r.store.rangeHook; hook != nil {
		if body, handled, err := hook(ctx, r.key, r.data, offset, length, attempt); handled {
			return body, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return engineBody(ctx, r.data[offset:offset+length]), nil
}

func (r *engineRanges) Close() error { r.closed.Store(true); return nil }

// engineContextBody reads bytes and fails once ctx ends, like the S3 store's
// cancellable bodies.
type engineContextBody struct {
	ctx    context.Context
	reader io.Reader
}

func engineBody(ctx context.Context, data []byte) io.ReadCloser {
	return &engineContextBody{ctx: ctx, reader: bytes.NewReader(data)}
}

func (b *engineContextBody) Read(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	return b.reader.Read(p)
}

func (b *engineContextBody) Close() error { return nil }

// engineBrokenBody returns data and then fails with err instead of EOF.
type engineBrokenBody struct {
	data []byte
	err  error
}

func (b *engineBrokenBody) Read(p []byte) (int, error) {
	if len(b.data) == 0 {
		return 0, b.err
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	return n, nil
}

func (b *engineBrokenBody) Close() error { return nil }

// engineStalledBody returns data and then blocks until ctx ends.
type engineStalledBody struct {
	ctx  context.Context
	data []byte
}

func (b *engineStalledBody) Read(p []byte) (int, error) {
	if len(b.data) > 0 {
		n := copy(p, b.data)
		b.data = b.data[n:]
		return n, nil
	}
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *engineStalledBody) Close() error { return nil }

// engineAPIError builds the error the S3 SDK returns for an error response.
func engineAPIError(status int, code, requestID string) error {
	response := &http.Response{StatusCode: status, Header: http.Header{}}
	return &smithy.OperationError{ServiceID: "S3", OperationName: "ListObjectsV2", Err: &awshttp.ResponseError{
		ResponseError: &smithyhttp.ResponseError{Response: &smithyhttp.Response{Response: response}, Err: &smithy.GenericAPIError{Code: code, Message: "test"}},
		RequestID:     requestID,
	}}
}

// engineOverride sets a package variable for one test.
func engineOverride[T any](t *testing.T, target *T, value T) {
	t.Helper()
	saved := *target
	*target = value
	t.Cleanup(func() { *target = saved })
}

// engineFastRetries removes the waits between retries.
func engineFastRetries(t *testing.T) {
	t.Helper()
	engineOverride(t, &listRetryDelays, []time.Duration{0, 0, 0, 0, 0})
	engineOverride(t, &rangeRetryDelays, []time.Duration{0, 0, 0})
	engineOverride(t, &streamRetryDelays, []time.Duration{0, 0})
}

// engineRun runs a comparison and returns every emitted row.
func engineRun(ctx context.Context, r Request, source, target transferstore.Store, wrap ReaderWrapper, options RunOptions) ([]Row, error) {
	var rows []Row
	err := Run(ctx, r, source, target, wrap, options, func(row Row) error {
		rows = append(rows, row)
		return nil
	})
	return rows, err
}

var engineLabelSequence atomic.Int64

// engineLabeled runs fn under a unique pprof label that every goroutine fn
// starts inherits, and returns the label value for engineNoGoroutines.
func engineLabeled(fn func(ctx context.Context)) string {
	id := strconv.FormatInt(engineLabelSequence.Add(1), 10)
	pprof.Do(context.Background(), pprof.Labels("comparison-engine-test", id), fn)
	return id
}

// engineNoGoroutines waits until no goroutine started under label is left.
// Run waits for its goroutines, but a context.AfterFunc callback started by a
// cancellation may still be returning, so this polls with a hang guard.
func engineNoGoroutines(t *testing.T, label string) {
	t.Helper()
	marker := `"comparison-engine-test":"` + label + `"`
	deadline := time.Now().Add(engineHangGuard)
	for {
		var profile bytes.Buffer
		if err := pprof.Lookup("goroutine").WriteTo(&profile, 1); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(profile.String(), marker) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines outlived Run:\n%s", profile.String())
		}
		time.Sleep(time.Millisecond)
	}
}

// engineWait blocks until done is closed, failing the test on a hang.
func engineWait(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(engineHangGuard):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// engineRowsByKey indexes rows by relative key.
func engineRowsByKey(t *testing.T, rows []Row) map[string]Row {
	t.Helper()
	result := map[string]Row{}
	for _, row := range rows {
		if _, ok := result[row.Key]; ok {
			t.Fatalf("key emitted twice: %+v", row)
		}
		result[row.Key] = row
	}
	return result
}

// engineAscending fails unless rows are in strictly ascending key order.
func engineAscending(t *testing.T, rows []Row) {
	t.Helper()
	for i := 1; i < len(rows); i++ {
		if rows[i-1].Key >= rows[i].Key {
			t.Fatalf("rows out of order at %d: %q then %q", i, rows[i-1].Key, rows[i].Key)
		}
	}
}
