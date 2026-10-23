package state_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func storageRecordFixture() execution.Record {
	when := time.Date(2026, 10, 21, 3, 4, 5, 123456789, time.UTC)
	full := transferstore.Object{
		Key: "目录/with space.bin", Size: 4096, ModifiedAt: when, ETag: `"opaque-part-2"`, VersionID: "version-17",
		ContentType: "application/octet-stream", ContentEncoding: "gzip", ContentDisposition: "attachment; filename=test.bin",
		CacheControl: "max-age=3600", StorageClass: "ARCHIVE", ExpiresAt: when.Add(time.Hour), LifecycleExpiresAt: when.Add(24 * time.Hour),
		Metadata: map[string]string{"source": "含空格 的值", "checksum": "a&b<c"},
	}
	resolved := full
	resolved.Key = "missing-at-discovery"
	return execution.Record{
		InventorySource: true,
		Job: execution.Job{
			ChildID: "17-3", ParentID: 17, Number: 3, Generation: 9, ConflictPolicy: "OVERWRITE", DiscoveryComplete: true,
			Source:      execution.Endpoint{Kind: "S3", Ref: "source", Bucket: "from"},
			Destination: execution.Endpoint{Kind: "S3", Ref: "target", Bucket: "to"},
			Items: []execution.Item{
				{ID: "pending", Source: transferstore.Object{Key: "pending", Size: 123}, DestinationKey: "copied/pending"},
				{ID: "success", Source: full, DestinationKey: full.Key, Legacy: &execution.LegacyCandidate{Successful: true, Missing: true, SourceChanged: true}},
				{ID: "resolved", Source: transferstore.Object{Key: resolved.Key}, DestinationKey: "copied/resolved"},
			},
		},
		Status: execution.Running, Attempt: 4, Fingerprint: "unchanged-stored-fingerprint", Error: "retained error",
		UpdatedAt: when, CreatedAt: when.Add(-time.Minute), StartedAt: &when,
		Results: []execution.ItemResult{
			{ID: "pending", Status: execution.ItemPending},
			{ID: "success", Status: execution.ItemSucceeded, Target: full, TargetCommitted: true, Reconciliation: "SHA256_VERIFIED"},
			{ID: "resolved", Status: execution.ItemFailed, ResolvedSource: &resolved, TargetCommitted: true, Error: "commit verification interrupted"},
		},
		Counters: execution.Counters{Total: 3, TotalBytes: 8315, Succeeded: 1, SucceededBytes: 4096, Failed: 1, FailedBytes: 4096},
	}
}

func assertStorageRoundTrip(t *testing.T, value any) {
	t.Helper()
	apiBefore, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	compact, err := json.Marshal(state.ForStorage(value))
	if err != nil {
		t.Fatal(err)
	}
	oldDecoded, compactDecoded := reflect.New(reflect.TypeOf(value)), reflect.New(reflect.TypeOf(value))
	if err := json.Unmarshal(apiBefore, oldDecoded.Interface()); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(compact, compactDecoded.Interface()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(oldDecoded.Interface(), compactDecoded.Interface()) {
		t.Fatalf("compact encoding changed persisted %T fields\nold: %s\nnew: %s", value, apiBefore, compact)
	}
	apiAfter, err := json.Marshal(value)
	if err != nil || !bytes.Equal(apiBefore, apiAfter) {
		t.Fatalf("storage encoding mutated API representation: %v", err)
	}
	// Public JSON writers remain unchanged. Child now reconstructs a storage-only
	// deduplicated job; schema 4 prevents older readers from opening those rows.
	if _, ok := value.(json.Marshaler); ok {
		t.Fatalf("%T now changes public JSON via MarshalJSON", value)
	}
	if _, child := value.(task.Child); !child {
		if _, ok := reflect.New(reflect.TypeOf(value)).Interface().(json.Unmarshaler); ok {
			t.Fatalf("%T unexpectedly requires a new JSON decoder", value)
		}
	}
}

func TestStorageEncodingPreservesMetadataAndOriginalJSONReaders(t *testing.T) {
	record := storageRecordFixture()
	child := task.Child{Job: record.Job, Node: task.Node{ID: "node-id", EnrollmentID: "enrollment", Address: "node-name", LocalRoot: "/data"}, Accepted: true, Record: &record}
	for name, value := range map[string]any{
		"object-full": record.Job.Items[1].Source,
		"object-zero": transferstore.Object{},
		"job":         record.Job, "record": record, "child": child,
		"job-nil-items": execution.Job{}, "job-empty-items": execution.Job{Items: []execution.Item{}},
		"record-nil-results": execution.Record{}, "record-empty-results": execution.Record{Results: []execution.ItemResult{}},
		"child-without-record": task.Child{Job: record.Job},
	} {
		t.Run(name, func(t *testing.T) { assertStorageRoundTrip(t, value) })
	}
	data, err := json.Marshal(state.ForStorage(child))
	if err != nil {
		t.Fatal(err)
	}
	// These fields are meaningful data even if rarely present in production.
	for _, key := range []string{"expiresAt", "lifecycleExpiresAt", "metadata", "resolvedSource", "legacy", "targetCommitted", "reconciliation", "fingerprint"} {
		if !bytes.Contains(data, []byte(`"`+key+`"`)) {
			t.Errorf("compact child lost %s", key)
		}
	}
}

func TestStorageEncodingOldAndNewRowsSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mixed.db")
	db, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	record := storageRecordFixture()
	child := task.Child{Job: record.Job, Node: task.Node{ID: "node", EnrollmentID: "original", Address: "worker"}, Accepted: true, Record: &record}
	values := map[string]any{"object": record.Job.Items[1].Source, "job": record.Job, "record": record, "child": child}
	if err := db.Update(func(tx *state.Tx) error {
		for name, value := range values {
			old, err := json.Marshal(value)
			if err != nil {
				return err
			}
			if err := tx.Put("old", name, json.RawMessage(old)); err != nil {
				return err
			}
			if err := tx.Put("compact", name, value); err != nil {
				return err
			}
			pointer := reflect.New(reflect.TypeOf(value))
			pointer.Elem().Set(reflect.ValueOf(value))
			if err := tx.Put("pointer", name, pointer.Interface()); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.View(func(tx *state.Tx) error {
		for name, value := range values {
			for _, bucket := range []string{"old", "compact", "pointer"} {
				decoded := reflect.New(reflect.TypeOf(value))
				found, err := tx.Get(bucket, name, decoded.Interface())
				if err != nil || !found || !reflect.DeepEqual(value, decoded.Elem().Interface()) {
					t.Fatalf("%s/%s changed on reopen: found=%v error=%v", bucket, name, found, err)
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Upgrade an existing long-form row in place, then read it without a new
	// schema or special decoder. Its exact semantic record must remain intact.
	if err := db.Update(func(tx *state.Tx) error {
		var old execution.Record
		if _, err := tx.Get("old", "record", &old); err != nil {
			return err
		}
		return tx.Put("old", "record", old)
	}); err != nil {
		t.Fatal(err)
	}
	var upgraded execution.Record
	if err := db.View(func(tx *state.Tx) error { _, err := tx.Get("old", "record", &upgraded); return err }); err != nil || !reflect.DeepEqual(upgraded, record) {
		t.Fatalf("old-row rewrite changed content: %v", err)
	}
}

func TestStorageEncodingRetainsGoldenJobFingerprintAndReplay(t *testing.T) {
	job := execution.Job{ChildID: "17-3", ParentID: 17, Number: 3, Generation: 9, ConflictPolicy: "OVERWRITE", DiscoveryComplete: true,
		Source: execution.Endpoint{Kind: "S3", Ref: "source", Bucket: "from"}, Destination: execution.Endpoint{Kind: "S3", Ref: "target", Bucket: "to"},
		Items: []execution.Item{{ID: "object-1", Source: transferstore.Object{Key: "folder/a.bin", Size: 4096, ETag: "opaque"}, DestinationKey: "copied/a.bin"}}}
	// Frozen public serialization from the original model, with the two
	// ownership fields normalized by the preexisting fingerprint algorithm.
	const original = `{"childId":"17-3","parentId":17,"number":3,"generation":0,"source":{"kind":"S3","ref":"source","bucket":"from"},"destination":{"kind":"S3","ref":"target","bucket":"to"},"conflictPolicy":"OVERWRITE","items":[{"id":"object-1","source":{"key":"folder/a.bin","size":4096,"modifiedAt":"0001-01-01T00:00:00Z","etag":"opaque","expiresAt":"0001-01-01T00:00:00Z","lifecycleExpiresAt":"0001-01-01T00:00:00Z"},"destinationKey":"copied/a.bin"}],"discoveryComplete":false}`
	canonical := job
	canonical.Generation, canonical.DiscoveryComplete = 0, false
	public, err := json.Marshal(canonical)
	if err != nil || string(public) != original {
		t.Fatalf("public job JSON changed, which changes historical fingerprints: %v\n%s", err, public)
	}
	sum := sha256.Sum256([]byte(original))
	want := hex.EncodeToString(sum[:])
	path := filepath.Join(t.TempDir(), "agent.db")
	db, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo, err := execution.NewRepository(db, execution.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := repo.Accept(context.Background(), job)
	if err != nil || first.Fingerprint != want {
		t.Fatalf("accepted fingerprint differs from original: got=%s want=%s err=%v", first.Fingerprint, want, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, err = execution.NewRepository(db, execution.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := repo.Accept(context.Background(), job)
	if err != nil || !reflect.DeepEqual(first, replayed) {
		t.Fatalf("compact storage changed immutable job identity after restart: %v", err)
	}
}

func TestStorageEncodingActuallyReducesPendingAndCompletedLedgers(t *testing.T) {
	for _, status := range []execution.ItemStatus{execution.ItemPending, execution.ItemSucceeded} {
		t.Run(string(status), func(t *testing.T) {
			record := execution.Record{Job: execution.Job{ChildID: "1-1", ParentID: 1, Number: 1, Generation: 1}, Status: execution.Running}
			for i := 0; i < 128; i++ {
				key := "folder/" + fmt.Sprintf("object-%05d.bin", i)
				object := transferstore.Object{Key: key, Size: 4096, ETag: strings.Repeat("a", 32), ModifiedAt: time.Date(2026, 10, 21, 3, 4, 5, 0, time.UTC)}
				record.Job.Items = append(record.Job.Items, execution.Item{ID: fmt.Sprint(i), Source: object, DestinationKey: key})
				result := execution.ItemResult{ID: fmt.Sprint(i), Status: status}
				if status == execution.ItemSucceeded {
					result.Target, result.TargetCommitted = object, true
				}
				record.Results = append(record.Results, result)
			}
			for _, value := range []any{record, task.Child{Job: record.Job, Record: &record}} {
				plain, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				compact, err := json.Marshal(state.ForStorage(value))
				if err != nil {
					t.Fatal(err)
				}
				if len(compact)*100 >= len(plain)*85 {
					t.Fatalf("%T %s storage reduction below 15%%: old=%d compact=%d", value, status, len(plain), len(compact))
				}
				t.Logf("%T %s old=%d compact=%d reduction=%.1f%%", value, status, len(plain), len(compact), 100*(1-float64(len(compact))/float64(len(plain))))
				assertStorageRoundTrip(t, value)
			}
		})
	}
}
