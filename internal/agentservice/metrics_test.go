package agentservice

import (
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
)

// The task speed counts the bytes child tasks report as sent to their
// destinations, from any number of concurrent writes, per task generation
// and over the last five complete seconds of an injected clock.
func TestTaskRateCountsBytesSentToTarget(t *testing.T) {
	m := NewTaskRates()
	now := time.Unix(1000, 0)
	m.now = func() time.Time { return now }
	job := execution.Job{ParentID: 1, Generation: 1, ChildID: "1-1"}
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			for range 5 {
				m.Add(job, 100)
			}
		})
	}
	wg.Wait()
	m.Add(job, 0)
	m.Add(job, -50)
	if rate := m.Snapshot(1, 1).BytesPerSecond; rate != 0 {
		t.Fatal("incomplete second counted", rate)
	}
	now = now.Add(time.Second)
	for range 3 { // Reading the API cannot reset or inflate the rate.
		s := m.Snapshot(1, 1)
		if s.BytesPerSecond != 1000 || s.Children["1-1"] != 1000 || s.Basis != execution.BasisTarget {
			t.Fatal(s)
		}
	}
	job.Generation = 2
	job.ChildID = "1-2"
	m.Add(job, 2500)
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
