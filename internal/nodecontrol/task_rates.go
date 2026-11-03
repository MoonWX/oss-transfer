package nodecontrol

import (
	"sync"
	"time"
)

// TaskRequestRates are admitted HTTP attempts per second, retries included,
// averaged over the last five complete seconds. GET counts object reads only;
// LIST includes ListParts. The task LIST budget admits LIST and GET.
type TaskRequestRates struct {
	List float64 `json:"list"`
	Get  float64 `json:"get"`
	Head float64 `json:"head"`
	Put  float64 `json:"put"`
}

// NodeRequestLimits are this node's effective request limits when the sample
// was taken. Zero means unlimited.
type NodeRequestLimits struct {
	List float64 `json:"list"`
	Head float64 `json:"head"`
	Put  float64 `json:"put"`
}

type TaskRequests struct {
	Rates      TaskRequestRates  `json:"rates"`
	NodeLimits NodeRequestLimits `json:"nodeLimits"`
}

type taskRequestKey struct {
	parent     int64
	generation uint64
}
type taskRequestBucket struct {
	second int64
	counts [4]int64
}
type taskRequestSeries struct {
	buckets [6]taskRequestBucket
	last    int64
}

// taskRequestCounter is memory only and separate from c.mu, so counting never
// extends the admission critical section shared with the node limiter.
type taskRequestCounter struct {
	mu        sync.Mutex
	now       func() time.Time
	lastPrune int64
	series    map[taskRequestKey]*taskRequestSeries
}

func newTaskRequestCounter() *taskRequestCounter {
	return &taskRequestCounter{now: time.Now, series: map[taskRequestKey]*taskRequestSeries{}}
}

func taskRequestIndex(operation string) int {
	switch operation {
	case "ListObjects", "ListBuckets", "ListParts":
		return 0
	case "GetObject":
		return 1
	case "HeadObject":
		return 2
	case "PutObject", "UploadPart", "CreateMultipartUpload", "CompleteMultipartUpload":
		return 3
	}
	return -1
}

func (m *taskRequestCounter) prune(second int64) {
	if second == m.lastPrune {
		return
	}
	m.lastPrune = second
	for key, series := range m.series {
		if second-series.last > 6 {
			delete(m.series, key)
		}
	}
}

func (m *taskRequestCounter) add(info RequestContext) {
	index := taskRequestIndex(info.Operation)
	if info.TaskID <= 0 || info.Generation == 0 || index < 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	second := m.now().Unix()
	m.prune(second)
	key := taskRequestKey{info.TaskID, info.Generation}
	series := m.series[key]
	if series == nil {
		series = &taskRequestSeries{}
		m.series[key] = series
	}
	bucket := &series.buckets[second%6]
	if bucket.second != second {
		*bucket = taskRequestBucket{second: second}
	}
	bucket.counts[index]++
	series.last = second
}

func (m *taskRequestCounter) rates(parent int64, generation uint64) TaskRequestRates {
	m.mu.Lock()
	defer m.mu.Unlock()
	second := m.now().Unix()
	m.prune(second)
	var counts [4]int64
	if series := m.series[taskRequestKey{parent, generation}]; series != nil {
		for _, bucket := range series.buckets {
			if bucket.second >= second-5 && bucket.second < second {
				for i, count := range bucket.counts {
					counts[i] += count
				}
			}
		}
	}
	return TaskRequestRates{List: float64(counts[0]) / 5, Get: float64(counts[1]) / 5, Head: float64(counts[2]) / 5, Put: float64(counts[3]) / 5}
}

// TaskRequests is cheap enough for every metrics sample: unlike Snapshot it
// never reads adjustment history from the database.
func (c *Controller) TaskRequests(parent int64, generation uint64) TaskRequests {
	out := TaskRequests{Rates: c.requests.rates(parent, generation)}
	c.mu.Lock()
	out.NodeLimits = NodeRequestLimits{List: c.data.Effective["list"], Head: c.data.Effective["head"], Put: c.data.Effective["put"]}
	c.mu.Unlock()
	return out
}
