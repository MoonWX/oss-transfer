package agentservice

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
)

func TestTaskRateCountsActualReadsAcrossStreamsAndGenerations(t *testing.T) {
	m := NewTaskRates()
	now := time.Unix(1000, 0)
	m.now = func() time.Time { return now }
	wrap := m.Decorator(func(_ context.Context, _ execution.Job, _ execution.Item, in io.ReadCloser) (io.ReadCloser, error) {
		return in, nil
	})
	job := execution.Job{ParentID: 1, Generation: 1, ChildID: "1-1"}
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := wrap(context.Background(), job, execution.Item{}, io.NopCloser(strings.NewReader(strings.Repeat("x", 500))))
			if err != nil {
				t.Error(err)
				return
			}
			defer r.Close()
			if n, err := io.Copy(io.Discard, r); err != nil || n != 500 {
				t.Error(n, err)
			}
		}()
	}
	wg.Wait()
	if rate := m.Snapshot(1, 1).BytesPerSecond; rate != 0 {
		t.Fatal("incomplete second counted", rate)
	}
	now = now.Add(time.Second)
	for range 3 { // Reading the API cannot reset or inflate the rate.
		s := m.Snapshot(1, 1)
		if s.BytesPerSecond != 1000 || s.Children["1-1"] != 1000 {
			t.Fatal(s)
		}
	}
	job.Generation = 2
	job.ChildID = "1-2"
	m.add(job, 2500)
	now = now.Add(time.Second)
	if m.Snapshot(1, 1).BytesPerSecond != 1000 || m.Snapshot(1, 2).BytesPerSecond != 500 || m.Snapshot(2, 1).BytesPerSecond != 0 {
		t.Fatal("generations or tasks were mixed")
	}
	now = now.Add(5 * time.Second)
	if s := m.Snapshot(1, 1); s.BytesPerSecond != 0 {
		t.Fatal("old sample survived", s)
	}
	now = now.Add(2 * time.Second)
	m.Snapshot(1, 1)
	if len(m.samples) != 0 {
		t.Fatal("inactive series leaked", len(m.samples))
	}
}
