package discovery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/state"
)

// This measures the durable scanner path, including compact indexes, metadata
// compression, source-identity reads, and fsync. No object payload is generated.
// Run with -run '^$' -bench BenchmarkScanCommitGroup -benchtime=1x.
func BenchmarkScanCommitGroup(b *testing.B) {
	const batchCount = 257
	for _, groupSize := range []int{8, 32, 64} {
		b.Run(fmt.Sprint(groupSize), func(b *testing.B) {
			for range b.N {
				path := filepath.Join(b.TempDir(), "agent.db")
				db, err := state.Open(path)
				if err != nil {
					b.Fatal(err)
				}
				if err := db.ClaimRole("agent"); err != nil {
					b.Fatal(err)
				}
				scanner := &Scanner{store: db}
				attempt := State{Spec: Spec{ParentID: 1, Generation: 1}, Generation: 1, Status: Scanning, Attempt: "0123456789abcdef0123456789abcdef"}
				if err := db.Update(func(tx *state.Tx) error { return tx.Put(states, parentKey(1), attempt) }); err != nil {
					b.Fatal(err)
				}
				commits := 0
				for first := 0; first < batchCount; {
					n := min(groupSize, batchCount-first)
					if first == 0 {
						n = 1
					}
					parts := make([]pendingBatch, n)
					for i := range parts {
						parts[i] = scanTestBatch(first + i)
					}
					if err := scanner.commitBatches(context.Background(), attempt, parts); err != nil {
						b.Fatal(err)
					}
					commits++
					first += n
				}
				got, err := scanner.Status(context.Background(), 1)
				if err != nil || got.TotalObjects != batchCount*128 || got.LastBatch != batchCount {
					b.Fatalf("totals: %+v %v", got, err)
				}
				if err := db.Close(); err != nil {
					b.Fatal(err)
				}
				info, err := os.Stat(path)
				if err != nil {
					b.Fatal(err)
				}
				b.ReportMetric(float64(commits), "durable-commits")
				b.ReportMetric(float64(info.Size())/(batchCount*128), "file-B/object")
			}
		})
	}
}
