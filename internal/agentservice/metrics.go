package agentservice

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
)

// TaskRates counts bytes read from source streams, including repeated reads.
// SDK retries of an already buffered upload body do not reread the source.
// Completed object sizes are not used: large objects commit in bursts.
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
	ParentID       int64              `json:"parentId"`
	Generation     uint64             `json:"generation"`
	BytesPerSecond float64            `json:"bytesPerSecond"`
	Children       map[string]float64 `json:"children"`
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
func (m *TaskRates) add(job execution.Job, amount int) {
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
	bucket.bytes += int64(amount)
	sample.last = second
}
func (m *TaskRates) Snapshot(parent int64, generation uint64) TaskRateSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	second := m.now().Unix()
	m.prune(second)
	out := TaskRateSnapshot{ParentID: parent, Generation: generation, Children: map[string]float64{}}
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

type taskRateReader struct {
	io.ReadCloser
	metrics *TaskRates
	job     execution.Job
}

func (r *taskRateReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.metrics.add(r.job, n)
	return n, err
}
func (m *TaskRates) Decorator(next execution.ReaderDecorator) execution.ReaderDecorator {
	return func(ctx context.Context, job execution.Job, item execution.Item, input io.ReadCloser) (io.ReadCloser, error) {
		reader, err := next(ctx, job, item, input)
		if err != nil {
			return nil, err
		}
		// Retain identifiers only, not the full manifest, in every reader.
		return &taskRateReader{reader, m, execution.Job{ParentID: job.ParentID, Generation: job.Generation, ChildID: job.ChildID}}, nil
	}
}
