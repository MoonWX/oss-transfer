package agentservice

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/ratelimit"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// Runs the actual durable Agent, local stores and scheduler. Kept opt-in because
// it measures elapsed time; correctness assertions do not depend on disk speed.
func TestAgentSchedulingPerformance(t *testing.T) {
	if os.Getenv("OSS_TRANSFER_PERFORMANCE") == "" {
		t.Skip("set OSS_TRANSFER_PERFORMANCE=1")
	}
	const children = 30
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	db, err := state.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := New(db, Options{Execution: execution.Options{MaxConcurrentJobs: 1, WorkersPerJob: 1}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	source, target := t.TempDir(), t.TempDir()
	data := make([]byte, 64<<10)
	for i := range data {
		data[i] = byte((i*17 + 3) % 251)
	}
	expected := sha256.Sum256(data)
	if err = os.WriteFile(filepath.Join(source, "data"), data, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := transferstore.NewLocal(source)
	if err != nil {
		t.Fatal(err)
	}
	object, err := store.Stat(ctx, "data")
	store.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Limits.ApplyParent(ctx, ratelimit.ParentLimit{ParentID: 1, Generation: 1, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	// Pause admission while assembling the identical batch in both versions.
	s.softwareMu.Lock()
	for i := 1; i <= children; i++ {
		job := execution.Job{ChildID: fmt.Sprintf("1-%d", i), ParentID: 1, Number: i, Generation: 1, Source: execution.Endpoint{Kind: "LOCAL", Root: source}, Destination: execution.Endpoint{Kind: "LOCAL", Root: target}, ConflictPolicy: "OVERWRITE", DiscoveryComplete: true, Items: []execution.Item{{ID: "data", Source: object, DestinationKey: fmt.Sprintf("%d", i)}}}
		if _, err = s.Engine.Accept(ctx, job); err != nil {
			s.softwareMu.Unlock()
			t.Fatal(err)
		}
	}
	start := time.Now()
	s.softwareMu.Unlock()
	s.signal()
	for i := 1; i <= children; i++ {
		for {
			rec, err := s.Engine.Get(ctx, fmt.Sprintf("1-%d", i))
			if err != nil {
				t.Fatal(err)
			}
			if rec.Status == execution.Completed {
				break
			}
			if rec.Counters.Failed > 0 {
				t.Fatalf("failed: %+v", rec)
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(time.Millisecond):
			}
		}
	}
	elapsed := time.Since(start)
	for i := 1; i <= children; i++ {
		got, err := os.ReadFile(filepath.Join(target, fmt.Sprint(i)))
		if err != nil || sha256.Sum256(got) != expected {
			t.Fatalf("child %d content mismatch: %v", i, err)
		}
	}
	t.Logf("children=%d bytes_per_child=%d concurrency=1 elapsed=%s children_per_second=%.2f sha256=all_match", children, len(data), elapsed, float64(children)/elapsed.Seconds())
}
