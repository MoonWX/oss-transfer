package master

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/task"
)

type taskRateCache struct {
	generation        uint64
	attempted         time.Time
	observed          time.Time
	valid             bool
	total             float64
	children          map[string]float64
	progressSupported bool
	progress          *task.ProgressProjection
}

// Missing Agent responses invalidate the aggregate. Showing a partial sum as
// the task speed would conceal an unreachable node that is still transferring.
func (t *taskRuntime) sampleRates(ctx context.Context, p task.Parent) {
	now := time.Now()
	t.mu.Lock()
	last := t.rates[p.ID]
	if last.generation == p.Generation && now.Sub(last.attempted) < time.Second {
		t.mu.Unlock()
		return
	}
	for id, entry := range t.rates {
		if now.Sub(entry.attempted) > 30*time.Second {
			delete(t.rates, id)
		}
	}
	entry := taskRateCache{generation: p.Generation, attempted: now, children: map[string]float64{}}
	if last.generation == p.Generation {
		// Keep the last complete sample visible while refreshing it. A failed
		// collection below invalidates it; changing generation invalidates now.
		last.attempted = now
		t.rates[p.ID] = last
	} else {
		t.rates[p.ID] = entry
	}
	t.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	type result struct {
		rate  agentservice.TaskRateSnapshot
		valid bool
	}
	nodes := p.ActiveNodes()
	results := make([]result, len(nodes))
	var workers sync.WaitGroup
	slots := make(chan struct{}, 8)
	for i, node := range nodes {
		workers.Add(1)
		go func() {
			defer workers.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				return
			}
			var sample agentservice.TaskRateSnapshot
			err := t.server.agentCall(ctx, nodeSession(node), "task_metrics", false, map[string]any{"parentId": p.ID, "generation": p.Generation}, &sample)
			if err != nil || sample.ParentID != p.ID || sample.Generation != p.Generation || !validTaskRate(sample.BytesPerSecond) {
				return
			}
			for _, rate := range sample.Children {
				if !validTaskRate(rate) {
					return
				}
			}
			results[i] = result{sample, true}
		}()
	}
	workers.Wait()
	entry.valid = len(results) > 0
	entry.progressSupported = entry.valid
	var progressReports []task.ProgressReport
	progressTruncated := false
	for index, result := range results {
		if !result.valid {
			entry.valid = false
			break
		}
		entry.progressSupported = entry.progressSupported && result.rate.ProgressSupported
		for _, child := range result.rate.Progress {
			if len(progressReports) >= task.MaxProgressReports {
				progressTruncated = true
				break
			}
			progressReports = append(progressReports, task.ProgressReport{NodeID: nodes[index].ID, EnrollmentID: nodes[index].EnrollmentID, Child: child})
		}
		entry.total += result.rate.BytesPerSecond
		for child, rate := range result.rate.Children {
			entry.children[child] += rate
		}
	}
	if entry.valid && entry.progressSupported && !progressTruncated {
		if projection, err := t.repo.ProjectProgress(p.ID, p.Generation, progressReports); err == nil {
			entry.progress = &projection
		}
	}
	entry.observed = time.Now()
	t.mu.Lock()
	if current := t.rates[p.ID]; current.generation == entry.generation && current.attempted.Equal(entry.attempted) {
		t.rates[p.ID] = entry
	}
	t.mu.Unlock()
}
func validTaskRate(rate float64) bool { return rate >= 0 && !math.IsNaN(rate) && !math.IsInf(rate, 0) }
func (t *taskRuntime) rate(p task.Parent, child string) (float64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.rates[p.ID]
	if !entry.valid || entry.generation != p.Generation || time.Since(entry.observed) > 10*time.Second {
		return 0, false
	}
	if child != "" {
		return entry.children[child], true
	}
	return entry.total, true
}
func (s *Server) taskView(p task.Parent, sets ...nodeDisplaySet) map[string]any {
	row := taskDTO(p)
	var displays nodeDisplaySet
	if len(sets) > 0 {
		displays = sets[0]
	} else {
		displays = s.nodeDisplaySnapshot()
	}
	displays.task(row, p.Spec.Nodes)
	s.addTaskRate(row, p, "")
	s.addLiveAverage(row, p, nil)
	return row
}
func (s *Server) childView(p task.Parent, c task.Child, sets ...nodeDisplaySet) map[string]any {
	row := childTaskDTO(p, c)
	var displays nodeDisplaySet
	if len(sets) > 0 {
		displays = sets[0]
	} else {
		displays = s.nodeDisplaySnapshot()
	}
	displays.task(row, []task.Node{c.Node})
	s.addTaskRate(row, p, c.Job.ChildID)
	s.addLiveAverage(row, p, &c)
	return row
}
func (s *Server) addTaskRate(row map[string]any, p task.Parent, child string) {
	row["currentSpeedBytesPerSecond"] = nil
	if task.Ended(p.Status) || task.Ended(stringValue(row["status"])) {
		row["currentSpeedBytesPerSecond"] = float64(0)
		return
	}
	if rate, ok := s.tasks.rate(p, child); ok {
		row["currentSpeedBytesPerSecond"] = rate
	}
}
func stringValue(value any) string { text, _ := value.(string); return text }
