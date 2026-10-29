package task

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"math/rand/v2"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
)

// Run explicitly with -run '^$' -bench BenchmarkPurge66027 -benchtime=1x.
// Setup is excluded from timing. Both algorithms remove the same persisted
// 66,027 child values and indexes; each child has 4 KiB of incompressible data.
// Each variant also performs an unrelated durable write every 5 ms. Reported
// cleanup commits exclude those probe commits; max_cleanup_step_ms includes
// writer acquisition, callback work, and durable commit. The 100 ms variant
// models coordinator pacing after node acknowledgements, not Agent/network time.
// The legacy implementation is retained here only as a performance baseline.
func BenchmarkPurge66027(b *testing.B) {
	for _, algorithm := range []string{"legacy_single_transaction", "bounded_raw_pages", "bounded_10ms_yield", "bounded_100ms_yield"} {
		b.Run(algorithm, func(b *testing.B) {
			var maxStep time.Duration
			var writerLatencies []time.Duration
			var cleanupCommits, probeCommits int64
			for range b.N {
				b.StopTimer()
				db, err := state.Open(filepath.Join(b.TempDir(), "master.db"))
				if err != nil {
					b.Fatal(err)
				}
				if err := db.ClaimRole("master"); err != nil {
					b.Fatal(err)
				}
				repo := New(db)
				p, _, err := repo.Create(testSpec(), "benchmark")
				if err != nil {
					b.Fatal(err)
				}
				random := rand.New(rand.NewPCG(11, 29))
				payload := make([]byte, 4096)
				for i := range payload {
					payload[i] = byte(random.Uint32())
				}
				encoded := base64.StdEncoding.EncodeToString(payload)
				for start := 1; start <= 66027; start += 2048 {
					if err := db.Update(func(tx *state.Tx) error {
						for i := start; i < min(start+2048, 66028); i++ {
							if err := tx.Put("master-task-children", childKey(p.ID, i), map[string]any{"number": i, "payload": encoded}); err != nil {
								return err
							}
							if err := tx.Put(allChildOrderBucket, childKey(p.ID, i), i); err != nil {
								return err
							}
						}
						return nil
					}); err != nil {
						b.Fatal(err)
					}
				}
				p.Status, p.Deleted, p.ChildCount = "COMPLETED", true, 66027
				if err := db.Update(func(tx *state.Tx) error { return repo.save(tx, p) }); err != nil {
					b.Fatal(err)
				}
				startProbe, stopProbe, probeDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
				probeErrors := make(chan error, 1)
				go func() {
					defer close(probeDone)
					<-startProbe
					ticker := time.NewTicker(5 * time.Millisecond)
					defer ticker.Stop()
					for {
						began := time.Now()
						value := probeCommits + 1
						if err := db.Update(func(tx *state.Tx) error { return tx.Put("unrelated-write", "heartbeat", value) }); err != nil {
							probeErrors <- err
							return
						}
						writerLatencies = append(writerLatencies, time.Since(began))
						probeCommits++
						select {
						case <-stopProbe:
							return
						case <-ticker.C:
						}
					}
				}()
				b.StartTimer()
				if algorithm != "legacy_single_transaction" {
					close(startProbe)
					_, _, err = repo.RequestPurge(p.ID)
					if err != nil {
						b.Fatal(err)
					}
					cleanupCommits++
					_, err = repo.UpdatePurge(p.ID, func(c *CleanupProgress) error {
						for id, node := range c.Nodes {
							node.Done = true
							c.Nodes[id] = node
						}
						c.Phase, c.Status = "MASTER", "RUNNING"
						return nil
					})
					if err != nil {
						b.Fatal(err)
					}
					cleanupCommits++
					for {
						began := time.Now()
						_, done, stepErr := repo.PurgePage(context.Background(), p.ID)
						maxStep = max(maxStep, time.Since(began))
						if stepErr != nil {
							err = stepErr
							break
						}
						cleanupCommits++
						if done {
							break
						}
						if algorithm == "bounded_100ms_yield" {
							time.Sleep(100 * time.Millisecond)
						} else if algorithm == "bounded_10ms_yield" {
							time.Sleep(10 * time.Millisecond)
						}
					}
				} else {
					began := time.Now()
					err = db.Update(func(tx *state.Tx) error {
						// Start the unrelated writer only after the legacy
						// cleanup owns the write lock, exposing the old stall.
						close(startProbe)
						for _, bucket := range cleanupBuckets {
							for {
								var keys []string
								if err := tx.Scan(bucket, key(p.ID)+"/", "", 128, func(k string, _ json.RawMessage) error { keys = append(keys, k); return nil }); err != nil {
									return err
								}
								for _, k := range keys {
									if err := tx.Delete(bucket, k); err != nil {
										return err
									}
								}
								if len(keys) < 128 {
									break
								}
							}
						}
						p.Purged = true
						return repo.save(tx, p)
					})
					maxStep = max(maxStep, time.Since(began))
					cleanupCommits++
				}
				close(stopProbe)
				<-probeDone
				select {
				case probeErr := <-probeErrors:
					b.Fatal(probeErr)
				default:
				}
				b.StopTimer()
				if err != nil {
					b.Fatal(err)
				}
				if err := db.Close(); err != nil {
					b.Fatal(err)
				}
			}
			sort.Slice(writerLatencies, func(i, j int) bool { return writerLatencies[i] < writerLatencies[j] })
			if len(writerLatencies) > 0 {
				b.ReportMetric(float64(writerLatencies[len(writerLatencies)-1].Microseconds())/1000, "writer_max_ms")
				b.ReportMetric(float64(writerLatencies[(len(writerLatencies)*95+99)/100-1].Microseconds())/1000, "writer_p95_ms")
			}
			b.ReportMetric(float64(maxStep.Microseconds())/1000, "max_cleanup_step_ms")
			b.ReportMetric(float64(cleanupCommits)/float64(b.N), "cleanup_commits/op")
			b.ReportMetric(float64(probeCommits)/float64(b.N), "unrelated_writes/op")
			b.ReportMetric(66027, "children/op")
		})
	}
}
