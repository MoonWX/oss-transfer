package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

type cleanupCountDB struct {
	db      *state.Store
	commits int
}

func (d *cleanupCountDB) View(fn func(*state.Tx) error) error { return d.db.View(fn) }
func (d *cleanupCountDB) Update(fn func(*state.Tx) error) error {
	err := d.db.Update(fn)
	if err == nil {
		d.commits++
	}
	return err
}

// Real bbolt transactions, compressed 128-object ledgers, and fsync remain on.
// Setup is excluded. Legacy retains the old full Record decode / 128-row path.
func BenchmarkExecutionCleanup(b *testing.B) {
	const records = 2048
	for _, mode := range []string{"legacy_full_record_128", "status_only_bounded"} {
		b.Run(mode, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				db, err := state.Open(filepath.Join(b.TempDir(), "agent.db"))
				if err != nil {
					b.Fatal(err)
				}
				if err := db.ClaimRole("agent"); err != nil {
					b.Fatal(err)
				}
				repo, err := NewRepository(db, RepositoryOptions{})
				if err != nil {
					b.Fatal(err)
				}
				record := Record{Job: Job{ParentID: 1, Generation: 1, ConflictPolicy: "SKIP", DiscoveryComplete: true}, Status: Completed}
				for n := 0; n < 128; n++ {
					key := fmt.Sprintf("dataset/episode/%04d/", n) + strings.Repeat("training-", 12) + "frame.bin"
					object := transferstore.Object{Key: key, Size: 1 << 20, ETag: strings.Repeat("a", 32), Metadata: map[string]string{"camera": "front", "sequence": "episode-001"}}
					record.Job.Items = append(record.Job.Items, Item{ID: fmt.Sprint(n), Source: object, DestinationKey: key})
					record.Results = append(record.Results, ItemResult{ID: fmt.Sprint(n), Status: ItemSucceeded, Target: object})
				}
				if err := db.Update(func(tx *state.Tx) error {
					for n := 1; n <= records; n++ {
						record.Job.Number = n
						record.Job.ChildID = fmt.Sprintf("1-%d", n)
						if err := tx.Put(jobsBucket, fmt.Sprintf("%020d/%020d", 1, n), record); err != nil {
							return err
						}
					}
					return tx.Put(parentsBucket, "1", ParentFence{ParentID: 1, Generation: 1})
				}); err != nil {
					b.Fatal(err)
				}
				if err := repo.fenceParent(context.Background(), 1, 1, true); err != nil {
					b.Fatal(err)
				}
				counter := &cleanupCountDB{db: db}
				repo.db = counter
				b.StartTimer()
				if mode == "legacy_full_record_128" {
					err = legacyCleanupBenchmark(counter)
				} else {
					err = repo.purgeDeleted(context.Background(), 1, 1)
				}
				b.StopTimer()
				if err != nil {
					b.Fatal(err)
				}
				var remaining int
				if err := db.View(func(tx *state.Tx) error {
					return tx.ScanKeys(jobsBucket, "00000000000000000001/", "", 1, func(string, int64) error { remaining++; return nil })
				}); err != nil || remaining != 0 {
					b.Fatalf("incomplete cleanup: %d %v", remaining, err)
				}
				b.ReportMetric(float64(counter.commits), "commits/op")
				if err := db.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func legacyCleanupBenchmark(db *cleanupCountDB) error {
	for {
		count := 0
		err := db.Update(func(tx *state.Tx) error {
			var keys []string
			if err := tx.Scan(jobsBucket, "00000000000000000001/", "", 128, func(key string, raw json.RawMessage) error {
				var r Record
				if err := json.Unmarshal(raw, &r); err != nil {
					return err
				}
				keys = append(keys, key)
				return nil
			}); err != nil {
				return err
			}
			for _, key := range keys {
				for _, bucket := range []string{jobsBucket, readyBucket, runningBucket} {
					if err := tx.Delete(bucket, key); err != nil {
						return err
					}
				}
			}
			count = len(keys)
			return nil
		})
		if err != nil || count < 128 {
			return err
		}
	}
}
