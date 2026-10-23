package discovery

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
	bolt "go.etcd.io/bbolt"
)

// This is the index-value encoder at baseline commit 892ad54. Public Object
// JSON intentionally differs; comparing that would overstate these savings.
func legacyIndexValue(row discoveredObject) ([]byte, error) {
	return json.Marshal(struct {
		Object      json.Marshaler `json:"object"`
		SeenAttempt string         `json:"seenAttempt"`
	}{state.ForStorage(row.Object), row.SeenAttempt})
}

func TestCompactDiscoveryIndexRetainsEveryIdentityFieldAndMissingSource(t *testing.T) {
	objectsToTest := []transferstore.Object{
		{Key: "zero-byte"},
		{Key: " leading/日本語/图片/😀\\name\n\t\"'?#%/ ", Size: 13, ModifiedAt: time.Date(2026, 10, 23, 1, 2, 3, 456, time.UTC), ETag: `"opaque-multipart-3"`, VersionID: "version",
			ContentType: "application/test", ContentEncoding: "gzip", ContentDisposition: "inline", CacheControl: "private", StorageClass: "ARCHIVE",
			ExpiresAt: time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC), LifecycleExpiresAt: time.Date(2028, 2, 3, 4, 5, 6, 7, time.UTC), Metadata: map[string]string{"k": "v", "日本語/中文": "😀\n\t\"quoted\"\\path", "empty": "", "<>&": "\x00\r\n"}},
	}
	db, err := state.Open(filepath.Join(t.TempDir(), "encoding.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for index, object := range objectsToTest {
		original := discoveredObject{Object: object, SeenAttempt: "abc012345678901234567890123456789"}
		legacy, err := legacyIndexValue(original)
		if err != nil {
			t.Fatal(err)
		}
		compact, err := original.MarshalStateJSON()
		if err != nil {
			t.Fatal(err)
		}
		if len(compact) >= len(legacy) {
			t.Fatalf("compact row grew: %d >= %d", len(compact), len(legacy))
		}
		for version, raw := range [][]byte{legacy, compact} {
			parentID := int64(index*2 + version + 1)
			if err := db.Update(func(tx *state.Tx) error {
				return tx.Put(objects, objectKey(parentID, object.Key), json.RawMessage(raw))
			}); err != nil {
				t.Fatal(err)
			}
			if err := db.View(func(tx *state.Tx) error {
				got, found, err := getDiscoveredObject(tx, parentID, object.Key)
				if err != nil || !found {
					return fmt.Errorf("index read found=%v: %w", found, err)
				}
				if !reflect.DeepEqual(got.Object, object) || got.SeenAttempt != original.SeenAttempt || execution.SourceIdentityUnknown(got.Object) != execution.SourceIdentityUnknown(object) {
					t.Fatalf("index codec changed source identity: %#v", got)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			converted, err := CompactObjectIndexValue(objectKey(parentID, object.Key), raw)
			if err != nil || string(converted) != string(compact) {
				t.Fatalf("offline conversion differs: %s %v", converted, err)
			}
		}
	}
	if _, err := CompactObjectIndexValue(objectKey(1, "wrong-key"), json.RawMessage(`{"object":{"key":"other"},"seenAttempt":"a"}`)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("conversion silently changed legacy key: %v", err)
	}
	if _, err := CompactObjectIndexValue(objectKey(1, "wrong-key"), json.RawMessage(`{}`)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("conversion accepted a missing object: %v", err)
	}
}

func TestOfflineIndexNormalizationPreservesUnknownFields(t *testing.T) {
	index := objectKey(1, "key")
	for _, raw := range []string{
		`{"object":{"key":"key","size":1},"seenAttempt":"a","future":{"x":[1,2]}}`,
		`{ "object": {"key":"key", "size":1,"futureIdentity":"keep"}, "seenAttempt":"a" }`,
		`{"o":{"s":1},"a":"a","future":true}`,
		`{"o":{"s":1,"future":{"retain":"日本語"}},"a":"a"}`,
		`{"o":{"s":1},"a":"a","object":{"key":"key"},"seenAttempt":"a"}`,
	} {
		got, err := CompactObjectIndexValue(index, json.RawMessage(raw))
		if err != nil || string(got) != raw {
			t.Fatalf("normalization discarded an unknown extension: %s %v", got, err)
		}
	}
	for _, index := range []string{"1/a2V5", "00000000000000000000/a2V5", "bad", parentKey(1) + "/%%%"} {
		if _, err := CompactObjectIndexValue(index, json.RawMessage(`{"o":{},"a":"a"}`)); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("normalization accepted malformed index %q: %v", index, err)
		}
	}
}

// Run explicitly with -run '^$' -bench BenchmarkDiscoveryIndexStorage
// -benchtime=1x. The six cases keep source data and 1024-row commit boundaries
// identical; they do not credit this change for earlier scan batching work.
func BenchmarkDiscoveryIndexStorage(b *testing.B) {
	const count = 100000
	for _, order := range []string{"sequential", "random"} {
		for _, variant := range []struct {
			name    string
			compact bool
			fill    float64
		}{
			{"baseline_892ad54", false, bolt.DefaultFillPercent}, {"compact", true, bolt.DefaultFillPercent}, {"compact_fill90", true, 0.9},
		} {
			b.Run(order+"/"+variant.name, func(b *testing.B) {
				for iteration := 0; iteration < b.N; iteration++ {
					path := filepath.Join(b.TempDir(), "index.db")
					db, err := bolt.Open(path, 0600, nil)
					if err != nil {
						b.Fatal(err)
					}
					permutation := make([]int, count)
					for i := range permutation {
						permutation[i] = i
					}
					if order == "random" {
						rand.New(rand.NewSource(42)).Shuffle(count, func(i, j int) { permutation[i], permutation[j] = permutation[j], permutation[i] })
					}
					var logical int64
					for start := 0; start < count; start += 1024 {
						err := db.Update(func(tx *bolt.Tx) error {
							bucket, err := tx.CreateBucketIfNotExists([]byte(objects))
							if err != nil {
								return err
							}
							bucket.FillPercent = variant.fill
							for _, number := range permutation[start:min(start+1024, count)] {
								key := fmt.Sprintf("training-data/scene-%05d/camera-01/frame-%09d.bin", number/1000, number)
								row := discoveredObject{Object: transferstore.Object{Key: key, Size: int64(1024 + number%65536), ModifiedAt: time.Date(2026, 10, 21, 1, 2, 3, 0, time.UTC), ETag: fmt.Sprintf("%032x", number)}, SeenAttempt: strings.Repeat("a", 32)}
								var raw []byte
								if variant.compact {
									raw, err = row.MarshalStateJSON()
								} else {
									raw, err = legacyIndexValue(row)
								}
								if err != nil {
									return err
								}
								index := objectKey(1, key)
								logical += int64(len(index) + len(raw))
								if err := bucket.Put([]byte(index), raw); err != nil {
									return err
								}
							}
							return nil
						})
						if err != nil {
							db.Close()
							b.Fatal(err)
						}
					}
					stats := db.Stats()
					var bucketStats bolt.BucketStats
					if err := db.View(func(tx *bolt.Tx) error { bucketStats = tx.Bucket([]byte(objects)).Stats(); return nil }); err != nil {
						b.Fatal(err)
					}
					if err := db.Close(); err != nil {
						b.Fatal(err)
					}
					file, err := os.Stat(path)
					if err != nil {
						b.Fatal(err)
					}
					b.ReportMetric(float64(logical)/count, "logical-B/object")
					b.ReportMetric(float64(file.Size())/count, "file-B/object")
					b.ReportMetric(float64(bucketStats.LeafAlloc+bucketStats.BranchAlloc)/count, "live-B/object")
					b.ReportMetric(float64(stats.TxStats.GetPageAlloc())/count, "allocated-B/object")
					b.ReportMetric(float64(stats.TxStats.GetWrite())/float64(count)*1000, "writes/1k-objects")
				}
			})
		}
	}
}
