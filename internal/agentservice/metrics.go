package agentservice

import (
	"sync"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/nodecontrol"
)

// TaskRates counts the bytes child tasks send to their destinations
// (execution.Options.CountTransferred): request bodies an S3 upload's HTTP
// attempts hand to the network, bytes sent again after a failed attempt
// included, and bytes written to local temporary files. Completed object
// sizes are not used: large objects commit in bursts. The node's bandwidth
// counts source reads.
type TaskRates struct {
	mu        sync.Mutex
	now       func() time.Time
	lastPrune int64
	samples   map[rateKey]*rateSeries
}
type rateKey struct {
	parent     int64
	generation uint64
	child      string
}
type rateBucket struct {
	second int64
	bytes  int64
}
type rateSeries struct {
	buckets [6]rateBucket
	last    int64
}
type TaskRateSnapshot struct {
	ParentID          int64                     `json:"parentId"`
	Generation        uint64                    `json:"generation"`
	BytesPerSecond    float64                   `json:"bytesPerSecond"`
	Children          map[string]float64        `json:"children"`
	ProgressSupported bool                      `json:"progressSupported"`
	Progress          []execution.ChildProgress `json:"progress,omitempty"`
	// Basis is TARGET: the speed and live progress count bytes sent to the
	// destination. Older Agents omit it; they count bytes read from the
	// source.
	Basis string `json:"basis,omitempty"`
	// Absent from older Agents; Master then reports observed requests as unknown.
	Requests *nodecontrol.TaskRequests `json:"requests,omitempty"`
}

func NewTaskRates() *TaskRates {
	return &TaskRates{now: time.Now, samples: make(map[rateKey]*rateSeries)}
}
func (m *TaskRates) prune(second int64) {
	if second == m.lastPrune {
		return
	}
	m.lastPrune = second
	for key, sample := range m.samples {
		if second-sample.last > 6 {
			delete(m.samples, key)
		}
	}
}

// Add counts amount bytes job's child task sent to its destination.
func (m *TaskRates) Add(job execution.Job, amount int64) {
	if amount <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	second := m.now().Unix()
	m.prune(second)
	key := rateKey{job.ParentID, job.Generation, job.ChildID}
	sample := m.samples[key]
	if sample == nil {
		sample = &rateSeries{}
		m.samples[key] = sample
	}
	bucket := &sample.buckets[second%6]
	if bucket.second != second {
		*bucket = rateBucket{second: second}
	}
	bucket.bytes += amount
	sample.last = second
}
func (m *TaskRates) Snapshot(parent int64, generation uint64) TaskRateSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	second := m.now().Unix()
	m.prune(second)
	out := TaskRateSnapshot{ParentID: parent, Generation: generation, Children: map[string]float64{}, Basis: execution.BasisTarget}
	for key, sample := range m.samples {
		if key.parent != parent || key.generation != generation {
			continue
		}
		var bytes int64
		for _, bucket := range sample.buckets {
			if bucket.second >= second-5 && bucket.second < second {
				bytes += bucket.bytes
			}
		}
		rate := float64(bytes) / 5
		out.Children[key.child] = rate
		out.BytesPerSecond += rate
	}
	return out
}
