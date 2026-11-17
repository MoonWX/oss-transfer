package comparison

import (
	"context"
	"slices"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// A side that could not be read, or that does not exist, has no size. Its row
// says so instead of reporting 0, which is the size of an empty object.
func TestEngineRowsMarkSidesWithoutAKnownSize(t *testing.T) {
	source, target := newEngineStore(), newEngineStore()
	listed := map[string]bool{}
	for _, key := range []string{"denied", "changed", "vanished", "unreadable", "missing"} {
		source.put("src/"+key, "data")
		listed["src/"+key] = true
	}
	for _, key := range []string{"extra", "extra-denied"} {
		target.put("backup/"+key, "xyz")
	}
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
	target.statHook = func(_ context.Context, key string) error {
		if key == "backup/denied" {
			return engineAPIError(403, "Forbidden", "T1")
		}
		return nil
	}
	source.statHook = func(_ context.Context, key string) error {
		switch key {
		case "src/changed":
			source.putObject(key, engineObject{data: []byte("DATA!"), etag: engineMD5ETag([]byte("DATA!"))})
		case "src/vanished":
			source.remove(key)
		case "src/unreadable", "src/extra-denied":
			return engineAPIError(500, "InternalError", "S1")
		}
		return nil
	}
	rows, err := engineRun(context.Background(), request(), source, target, nil, RunOptions{Workers: 3})
	if err != nil {
		t.Fatal(err)
	}
	got := engineRowsByKey(t, rows)
	for key, want := range map[string]struct {
		status                   string
		sourceSize               int64
		sourceKnown, targetKnown bool
		targetSize               int64
	}{
		// The target HEAD failed: the target size is unknown.
		"denied": {StatusError, 4, true, false, 0},
		// The target does not exist; the source changed or went away.
		"changed":  {StatusChanged, 4, true, false, 0},
		"vanished": {StatusChanged, 4, true, false, 0},
		// The target does not exist, and the source HEAD failed.
		"unreadable": {StatusError, 4, true, false, 0},
		// The source HEAD failed for a key listed only on the target.
		"extra-denied": {StatusError, 0, false, true, 3},
		// These statuses say themselves which side is missing.
		"missing": {StatusMissingTarget, 4, true, true, 0},
		"extra":   {StatusExtraTarget, 0, true, true, 3},
	} {
		row := got[key]
		if row.Status != want.status || row.SourceSize != want.sourceSize || row.DestinationSize != want.targetSize ||
			row.SourceSizeUnknown == want.sourceKnown || row.DestinationSizeUnknown == want.targetKnown {
			t.Errorf("%s: got %+v", key, row)
		}
	}
}

// A single-object comparison whose target HEAD fails still knows the size of
// the source it read.
func TestEngineSingleObjectErrorKeepsTheSizeItRead(t *testing.T) {
	source, target := newEngineStore(), newEngineStore()
	source.put("src/a.bin", "data")
	target.put("backup/a.bin", "data")
	target.statHook = func(context.Context, string) error { return engineAPIError(403, "Forbidden", "T1") }
	r := request()
	r.Scope, r.SourcePrefix, r.DestinationPrefix = "object", "src/a.bin", "backup/a.bin"
	rows, err := engineRun(context.Background(), r, source, target, nil, RunOptions{})
	if err != nil || len(rows) != 1 {
		t.Fatal(err, rows)
	}
	if row := rows[0]; row.Status != StatusError || row.SourceSize != 4 || row.SourceSizeUnknown || !row.DestinationSizeUnknown {
		t.Fatalf("single-object read failure: %+v", row)
	}
}
