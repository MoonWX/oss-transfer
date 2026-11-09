package comparison

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// engineMixedStores builds count keys with every kind of merge result and
// returns the expected status and reasons per relative key.
func engineMixedStores(count int) (source, target *engineStore, want map[string]string) {
	source, target, want = newEngineStore(), newEngineStore(), map[string]string{}
	for i := 0; i < count; i++ {
		key := fmt.Sprintf("%04d", i)
		source.put("src/"+key, "x")
		switch {
		case i%7 == 0:
			want[key] = StatusMissingTarget
		case i%11 == 0:
			target.put("backup/"+key, "xy")
			want[key] = StatusDifferent + ":" + ReasonSize
		case i%13 == 0:
			target.putObject("backup/"+key, engineObject{data: []byte("x"), etag: engineMD5ETag([]byte("x")), contentType: "text/plain"})
			want[key] = StatusDifferent + ":" + ReasonContentType
		default:
			target.put("backup/"+key, "x")
			want[key] = StatusMatch
		}
		if i%17 == 0 {
			target.put("backup/"+key+"-extra", "e")
			want[key+"-extra"] = StatusExtraTarget
		}
	}
	return source, target, want
}

func engineOutcome(row Row) string {
	if len(row.Reasons) == 0 {
		return row.Status
	}
	return row.Status + ":" + strings.Join(row.Reasons, ",")
}

func TestEngineConcurrentRowsKeepKeyOrder(t *testing.T) {
	source, target, want := engineMixedStores(600)
	// Seeded per-key HEAD delays make later keys finish before earlier ones.
	random := rand.New(rand.NewPCG(7, 11))
	delays := map[string]time.Duration{}
	for i := 0; i < 700; i++ {
		delays[fmt.Sprintf("%04d", i)] = time.Duration(random.IntN(400)) * time.Microsecond
	}
	delay := func(_ context.Context, key string) error {
		relative := strings.TrimPrefix(strings.TrimPrefix(key, "src/"), "backup/")
		time.Sleep(delays[strings.TrimSuffix(relative, "-extra")])
		return nil
	}
	source.statHook, target.statHook = delay, delay
	serial, err := engineRun(context.Background(), request(), source, target, nil, RunOptions{Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	concurrent, err := engineRun(context.Background(), request(), source, target, nil, RunOptions{Workers: 6})
	if err != nil {
		t.Fatal(err)
	}
	engineAscending(t, concurrent)
	if !reflect.DeepEqual(serial, concurrent) {
		t.Fatal("concurrent comparison emitted different rows than a serial one")
	}
	got := engineRowsByKey(t, concurrent)
	if len(got) != len(want) {
		t.Fatalf("emitted %d rows, want %d", len(got), len(want))
	}
	for key, outcome := range want {
		if engineOutcome(got[key]) != outcome {
			t.Fatalf("key %s: got %+v, want %s", key, got[key], outcome)
		}
	}
	if got["0000"].SourceKey != "src/0000" || got["0000"].DestinationKey != "backup/0000" || got["0017-extra"].SourceKey != "src/0017-extra" {
		t.Fatalf("relative key mapping lost: %+v %+v", got["0000"], got["0017-extra"])
	}
}

func TestEngineBoundsKeysInFlightAndUnemitted(t *testing.T) {
	const workers, window, count = 2, 8, 64
	source, target := newEngineStore(), newEngineStore()
	for i := 0; i < count; i++ {
		source.put(fmt.Sprintf("src/%04d", i), "x")
		target.put(fmt.Sprintf("backup/%04d", i), "x")
	}
	var started, finished, emitted, worst atomic.Int64
	release, blocked := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	allOthers := make(chan struct{})
	source.statHook = func(ctx context.Context, key string) error {
		// compare reads the source first, so this sees each key start.
		outstanding := started.Add(1) - emitted.Load()
		for {
			previous := worst.Load()
			if outstanding <= previous || worst.CompareAndSwap(previous, outstanding) {
				break
			}
		}
		if key == "src/0000" {
			close(blocked)
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return nil
	}
	target.statHook = func(_ context.Context, key string) error {
		if key != "backup/0000" && finished.Add(1) == window-1 {
			close(allOthers)
		}
		return nil
	}
	done := make(chan struct{})
	var rows []Row
	var runErr error
	go func() {
		defer close(done)
		runErr = Run(context.Background(), request(), source, target, nil, RunOptions{Workers: workers}, func(row Row) error {
			emitted.Add(1)
			rows = append(rows, row)
			return nil
		})
	}()
	engineWait(t, blocked, "the first key")
	// Every other key of the first window finishes while the first key is
	// held, and none may be emitted before it.
	engineWait(t, allOthers, "the rest of the window")
	if got := started.Load(); got != window || emitted.Load() != 0 {
		t.Fatalf("started %d keys and emitted %d while the first key was held; the window is %d", got, emitted.Load(), window)
	}
	releaseOnce.Do(func() { close(release) })
	engineWait(t, done, "the comparison")
	if runErr != nil || len(rows) != count {
		t.Fatalf("comparison failed: %v, %d rows", runErr, len(rows))
	}
	engineAscending(t, rows)
	if worst.Load() > window {
		t.Fatalf("%d keys were outstanding at once; the bound is %d", worst.Load(), window)
	}
}

func TestEngineStartAfterSkipsBothSides(t *testing.T) {
	source, target := newEngineStore(), newEngineStore()
	for _, key := range []string{"a", "b", "c", "d"} {
		source.put("src/"+key, key)
	}
	for _, key := range []string{"a", "b", "c", "e"} {
		target.put("backup/"+key, key)
	}
	rows, err := engineRun(context.Background(), request(), source, target, nil, RunOptions{StartAfter: "b", Workers: 3})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, row := range rows {
		got = append(got, row.Key+"="+row.Status)
	}
	if want := []string{"c=" + StatusMatch, "d=" + StatusMissingTarget, "e=" + StatusExtraTarget}; !slices.Equal(got, want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	for _, check := range []struct {
		store *engineStore
		want  string
	}{{source, "src/b"}, {target, "backup/b"}} {
		check.store.mu.Lock()
		for _, options := range check.store.listLog {
			if options.StartAfter != check.want || options.Limit != 1000 {
				t.Errorf("listed with %+v, want StartAfter %q and 1000 keys a page", options, check.want)
			}
		}
		check.store.mu.Unlock()
	}

	// A listing that ignores the start key cannot be resumed safely.
	ignoring := newEngineStore()
	ignoring.put("src/a", "a")
	ignoring.put("src/c", "c")
	ignoring.listHook = func(_ context.Context, options transferstore.ListOptions, _ int) (transferstore.Page, bool, error) {
		return transferstore.Page{Objects: []transferstore.Object{{Key: "src/a", Size: 1}, {Key: "src/c", Size: 1}}}, true, nil
	}
	rows, err = engineRun(context.Background(), request(), ignoring, target, nil, RunOptions{StartAfter: "b"})
	if want := `source listing is not in ascending byte order after key "b"`; err == nil || err.Error() != want || ErrorDetail(err) != want || len(rows) != 0 {
		t.Fatalf("listing before the start key was accepted: %v %+v", err, rows)
	}
}

// engineStaticListing serves fixed pages: pages[i] answers the i-th call.
func engineStaticListing(pages ...transferstore.Page) func(context.Context, transferstore.ListOptions, int) (transferstore.Page, bool, error) {
	return func(_ context.Context, _ transferstore.ListOptions, call int) (transferstore.Page, bool, error) {
		if call > len(pages) {
			return transferstore.Page{}, true, errors.New("listed past the last page")
		}
		return pages[call-1], true, nil
	}
}

func engineObjects(keys ...string) []transferstore.Object {
	objects := make([]transferstore.Object, 0, len(keys))
	for _, key := range keys {
		objects = append(objects, transferstore.Object{Key: key, Size: 1, ETag: engineMD5ETag([]byte("x"))})
	}
	return objects
}

func TestEngineRejectsUnusableListings(t *testing.T) {
	long := strings.Repeat("k", 300)
	for _, test := range []struct {
		name         string
		sourcePages  []transferstore.Page
		targetPages  []transferstore.Page
		want         string
		maxEmittedTo string
	}{
		{"unordered", []transferstore.Page{{Objects: engineObjects("src/a", "src/c", "src/b")}}, nil, `source listing is not in ascending byte order after key "c"`, "c"},
		{"duplicate across pages", []transferstore.Page{{Objects: engineObjects("src/a"), NextCursor: "1"}, {Objects: engineObjects("src/a")}}, nil, `source listing is not in ascending byte order after key "a"`, "a"},
		{"outside prefix", []transferstore.Page{{Objects: engineObjects("src/a", "other/b")}}, nil, "source listing returned a key outside the prefix", "a"},
		{"target unordered", nil, []transferstore.Page{{Objects: engineObjects("backup/b", "backup/a")}}, `target listing is not in ascending byte order after key "b"`, "b"},
		{"not advancing", []transferstore.Page{{Objects: engineObjects("src/a"), NextCursor: "same"}, {Objects: engineObjects("src/b"), NextCursor: "same"}}, nil, "source listing did not advance", "a"},
		{"long key clipped", []transferstore.Page{{Objects: engineObjects("src/"+long, "src/a")}}, nil, fmt.Sprintf("source listing is not in ascending byte order after key %q", long[:253]+"..."), long},
	} {
		t.Run(test.name, func(t *testing.T) {
			source, target := newEngineStore(), newEngineStore()
			if test.sourcePages != nil {
				source.listHook = engineStaticListing(test.sourcePages...)
			}
			if test.targetPages != nil {
				target.listHook = engineStaticListing(test.targetPages...)
			}
			var rows []Row
			var err error
			label := engineLabeled(func(ctx context.Context) {
				rows, err = engineRun(ctx, request(), source, target, nil, RunOptions{Workers: 2})
			})
			engineNoGoroutines(t, label)
			if err == nil || err.Error() != test.want || ErrorDetail(err) != test.want {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			engineAscending(t, rows)
			for _, row := range rows {
				if row.Key > test.maxEmittedTo {
					t.Fatalf("emitted a row past the unusable listing: %+v", row)
				}
			}
		})
	}
}

func TestEngineListFailureEmitsOrderedPrefix(t *testing.T) {
	source, target := newEngineStore(), newEngineStore()
	for i := 0; i < 3000; i++ {
		source.put(fmt.Sprintf("src/%04d", i), "x")
		target.put(fmt.Sprintf("backup/%04d", i), "x")
	}
	var failedCalls atomic.Int64
	source.listHook = func(_ context.Context, options transferstore.ListOptions, _ int) (transferstore.Page, bool, error) {
		if options.Cursor == "src/1999" {
			failedCalls.Add(1)
			return transferstore.Page{}, true, engineAPIError(403, "AccessDenied", "REQ-PREFIX")
		}
		return transferstore.Page{}, false, nil
	}
	rows, err := engineRun(context.Background(), request(), source, target, nil, RunOptions{Workers: 4})
	if want := "source LIST: HTTP 403 AccessDenied (request ID REQ-PREFIX)"; err == nil || ErrorDetail(err) != want {
		t.Fatalf("error detail = %q (%v), want %q", ErrorDetail(err), err, want)
	}
	if failedCalls.Load() != 1 {
		t.Fatalf("an access denial was retried %d times", failedCalls.Load()-1)
	}
	if len(rows) > 2000 {
		t.Fatalf("emitted %d rows beyond the failed page", len(rows))
	}
	for i, row := range rows {
		if row.Key != fmt.Sprintf("%04d", i) || row.Status != StatusMatch {
			t.Fatalf("row %d is not the next key in order: %+v", i, row)
		}
	}
}

func TestEngineListRetriesTransientFailures(t *testing.T) {
	engineFastRetries(t)
	engineOverride(t, &listAttemptTimeout, 200*time.Millisecond)
	source, target := newEngineStore(), newEngineStore()
	source.put("src/a", "a")
	target.put("backup/a", "a")
	var stalledErr error
	source.listHook = func(ctx context.Context, _ transferstore.ListOptions, call int) (transferstore.Page, bool, error) {
		switch call {
		case 1:
			return transferstore.Page{}, true, engineAPIError(500, "InternalError", "R1")
		case 2:
			return transferstore.Page{}, true, engineAPIError(503, "SlowDown", "R2")
		case 3:
			return transferstore.Page{}, true, &smithyhttp.RequestSendError{Err: errors.New("connection reset by peer")}
		case 4:
			// A stalled LIST ends with its attempt deadline and is retried.
			<-ctx.Done()
			stalledErr = ctx.Err()
			return transferstore.Page{}, true, ctx.Err()
		}
		return transferstore.Page{}, false, nil
	}
	rows, err := engineRun(context.Background(), request(), source, target, nil, RunOptions{})
	if err != nil || len(rows) != 1 || rows[0].Status != StatusMatch {
		t.Fatalf("transient LIST failures were not retried: %v %+v", err, rows)
	}
	if source.lists.Load() != 5 || !errors.Is(stalledErr, context.DeadlineExceeded) {
		t.Fatalf("expected 5 LIST attempts with one attempt deadline, got %d (%v)", source.lists.Load(), stalledErr)
	}

	for _, test := range []struct {
		name string
		hook func(context.Context) error
		want string
	}{
		{"server errors", func(context.Context) error { return engineAPIError(500, "InternalError", "R500") }, "source LIST: HTTP 500 InternalError (request ID R500)"},
		{"attempt timeouts", func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }, "source LIST: timeout"},
	} {
		t.Run(test.name, func(t *testing.T) {
			failing := newEngineStore()
			failing.listHook = func(ctx context.Context, _ transferstore.ListOptions, _ int) (transferstore.Page, bool, error) {
				return transferstore.Page{}, true, test.hook(ctx)
			}
			_, err := engineRun(context.Background(), request(), failing, target, nil, RunOptions{})
			if ErrorDetail(err) != test.want || failing.lists.Load() != int64(1+len(listRetryDelays)) {
				t.Fatalf("got %q after %d attempts, want %q after %d", ErrorDetail(err), failing.lists.Load(), test.want, 1+len(listRetryDelays))
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("a LIST failure looks like cancellation: %v", err)
			}
		})
	}
}

func TestEngineEmitErrorStopsAtOnce(t *testing.T) {
	source, target, _ := engineMixedStores(500)
	stop := errors.New("cannot save")
	var rows []Row
	err := Run(context.Background(), request(), source, target, nil, RunOptions{Workers: 5}, func(row Row) error {
		rows = append(rows, row)
		if len(rows) == 100 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) || len(rows) != 100 {
		t.Fatalf("emit error not returned at once: %v after %d rows", err, len(rows))
	}
	engineAscending(t, rows)
}

func TestEngineCancellationStopsWithoutGoroutines(t *testing.T) {
	for _, mode := range []string{"metadata", "content"} {
		t.Run(mode, func(t *testing.T) {
			engineOverride(t, &contentRangeSize, 1024)
			source, target := newEngineStore(), newEngineStore()
			ranged := &engineRangeStore{engineStore: target}
			data := strings.Repeat("d", 10_000)
			for i := 0; i < 200; i++ {
				source.put(fmt.Sprintf("src/%04d", i), data)
				target.put(fmt.Sprintf("backup/%04d", i), data)
			}
			var blocked atomic.Int64
			enough := make(chan struct{})
			var enoughOnce sync.Once
			hold := func(ctx context.Context) {
				if blocked.Add(1) >= 3 {
					enoughOnce.Do(func() { close(enough) })
				}
				<-ctx.Done()
			}
			if mode == "metadata" {
				target.statHook = func(ctx context.Context, key string) error {
					if key >= "backup/0005" {
						hold(ctx)
						return ctx.Err()
					}
					return nil
				}
			} else {
				// Source objects stream with Open; target ranges stall.
				ranged.rangeHook = func(ctx context.Context, _ string, data []byte, offset, length int64, _ int) (io.ReadCloser, bool, error) {
					hold(ctx)
					return &engineStalledBody{ctx: ctx, data: data[offset : offset+length/2]}, true, nil
				}
			}
			r := request()
			r.Mode = mode
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var rows []Row
			var err error
			done := make(chan struct{})
			label := engineLabeled(func(labeled context.Context) {
				go func() {
					defer close(done)
					rows, err = engineRun(ctx, r, source, ranged, nil, RunOptions{Workers: 4})
				}()
			})
			engineWait(t, enough, "blocked storage calls")
			cancel()
			engineWait(t, done, "Run to return after cancellation")
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Run returned %v after cancellation", err)
			}
			engineAscending(t, rows)
			for i, row := range rows {
				if row.Key != fmt.Sprintf("%04d", i) || row.Status != StatusMatch {
					t.Fatalf("emitted a row computed under cancellation: %+v", row)
				}
			}
			engineNoGoroutines(t, label)
		})
	}
}

func TestEngineObjectScope(t *testing.T) {
	source, target := newEngineStore(), newEngineStore()
	source.put("src/same", "data")
	target.put("backup/same", "data")
	source.put("src/only-source", "data")
	target.put("backup/only-target", "target")
	source.put("src/meta", "data")
	target.putObject("backup/meta", engineObject{data: []byte("data"), etag: engineMD5ETag([]byte("data")), contentType: "text/plain", metadata: map[string]string{"k": "v"}})
	source.put("src/denied", "data")
	target.put("backup/denied", "data")
	target.statHook = func(_ context.Context, key string) error {
		if key == "backup/denied" || key == "backup/denied-missing" {
			return engineAPIError(403, "Forbidden", "H1")
		}
		return nil
	}
	compare := func(sourceKey, targetKey string) Row {
		t.Helper()
		r := request()
		r.Scope, r.SourcePrefix, r.DestinationPrefix = "object", sourceKey, targetKey
		rows, err := engineRun(context.Background(), r, source, target, nil, RunOptions{StartAfter: "ignored"})
		if err != nil || len(rows) != 1 || rows[0].Key != sourceKey || rows[0].SourceKey != sourceKey || rows[0].DestinationKey != targetKey {
			t.Fatalf("object scope failed: %v %+v", err, rows)
		}
		return rows[0]
	}
	if row := compare("src/same", "backup/same"); row.Status != StatusMatch {
		t.Fatalf("same object: %+v", row)
	}
	if row := compare("src/none", "backup/none"); row.Status != StatusBothMissing {
		t.Fatalf("both missing: %+v", row)
	}
	if row := compare("src/only-source", "backup/none"); row.Status != StatusMissingTarget || row.SourceSize != 4 {
		t.Fatalf("missing target: %+v", row)
	}
	if row := compare("src/none", "backup/only-target"); row.Status != StatusExtraTarget || row.DestinationSize != 6 {
		t.Fatalf("extra target: %+v", row)
	}
	if row := compare("src/meta", "backup/meta"); row.Status != StatusDifferent || !slices.Equal(row.Reasons, []string{ReasonContentType, ReasonMetadata}) {
		t.Fatalf("metadata difference: %+v", row)
	}
	want := "target HEAD: HTTP 403 Forbidden (request ID H1)"
	if row := compare("src/denied", "backup/denied"); row.Status != StatusError || row.Error != "Cannot read object metadata" || row.ErrorDetail != want {
		t.Fatalf("denied HEAD: %+v", row)
	}
	if row := compare("src/none", "backup/denied-missing"); row.Status != StatusError || row.ErrorDetail != want {
		t.Fatalf("a missing source hid the target error: %+v", row)
	}
}

func TestEngineOneSidedKeysAreConfirmedWithHead(t *testing.T) {
	source, target := newEngineStore(), newEngineStore()
	listed := map[string]bool{}
	for _, key := range []string{"raced", "denied", "changed", "vanished", "unreadable", "missing"} {
		source.put("src/"+key, "data")
		listed["src/"+key] = true
	}
	target.put("backup/raced", "data") // present, but absent from the target listing
	target.put("backup/extra", "x")
	target.listHook = func(context.Context, transferstore.ListOptions, int) (transferstore.Page, bool, error) {
		return transferstore.Page{Objects: engineObjects("backup/extra")}, true, nil
	}
	target.statHook = func(_ context.Context, key string) error {
		if key == "backup/denied" {
			return engineAPIError(403, "Forbidden", "T1")
		}
		return nil
	}
	// The listing describes every source object as it was before the HEADs
	// below change or remove some of them.
	source.listHook = func(context.Context, transferstore.ListOptions, int) (transferstore.Page, bool, error) {
		var keys []string
		for key := range listed {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		objects := make([]transferstore.Object, 0, len(keys))
		for _, key := range keys {
			objects = append(objects, transferstore.Object{Key: key, Size: 4, ETag: engineMD5ETag([]byte("data"))})
		}
		return transferstore.Page{Objects: objects}, true, nil
	}
	source.statHook = func(_ context.Context, key string) error {
		switch key {
		case "src/changed":
			source.putObject(key, engineObject{data: []byte("DATA"), etag: engineMD5ETag([]byte("DATA"))})
		case "src/vanished":
			source.remove(key)
		case "src/unreadable":
			return engineAPIError(500, "InternalError", "S1")
		}
		return nil
	}
	rows, err := engineRun(context.Background(), request(), source, target, nil, RunOptions{Workers: 3})
	if err != nil {
		t.Fatal(err)
	}
	got := engineRowsByKey(t, rows)
	for key, want := range map[string]string{
		"raced":      StatusMatch,
		"denied":     StatusError,
		"changed":    StatusChanged,
		"vanished":   StatusChanged,
		"unreadable": StatusError,
		"missing":    StatusMissingTarget,
		"extra":      StatusExtraTarget,
	} {
		if got[key].Status != want {
			t.Fatalf("%s: got %+v, want %s", key, got[key], want)
		}
	}
	if row := got["denied"]; row.Error != "Cannot read target metadata" || row.ErrorDetail != "target HEAD: HTTP 403 Forbidden (request ID T1)" {
		t.Fatalf("target HEAD failure lost its detail: %+v", row)
	}
	if row := got["unreadable"]; row.Error != "Cannot read source metadata" || row.ErrorDetail != "source HEAD: HTTP 500 InternalError (request ID S1)" {
		t.Fatalf("source HEAD failure lost its detail: %+v", row)
	}
	if row := got["missing"]; row.SourceSize != 4 || row.DestinationKey != "backup/missing" {
		t.Fatalf("missing target row: %+v", row)
	}
}

func TestEngineHeadTimeoutIsAnErrorRow(t *testing.T) {
	engineOverride(t, &headTimeout, 100*time.Millisecond)
	source, target := newEngineStore(), newEngineStore()
	source.put("src/a", "a")
	target.put("backup/a", "a")
	target.statHook = func(ctx context.Context, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	}
	rows, err := engineRun(context.Background(), request(), source, target, nil, RunOptions{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("a HEAD timeout failed the comparison: %v %+v", err, rows)
	}
	if row := rows[0]; row.Status != StatusError || row.Error != "Cannot read object metadata" || row.ErrorDetail != "target HEAD: timeout" {
		t.Fatalf("HEAD timeout row: %+v", row)
	}
}
