package master

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/nodecontrol"
	"github.com/MoonWX/oss-transfer/internal/task"
)

// Both task speeds describe the same aggregate observation. File-level
// progress has a shorter lifetime because it also identifies active objects.
const taskRateMaxAge = 10 * time.Second

type taskRateCache struct {
	generation        uint64
	attempted         time.Time
	observed          time.Time
	valid             bool
	total             float64
	children          map[string]float64
	progressSupported bool
	progress          *task.ProgressProjection
	// Nil unless every node reported requests: a partial sum would read as a
	// slower task rather than an unknown one.
	requests   *nodecontrol.TaskRequestRates
	nodeLimits map[string]nodecontrol.NodeRequestLimits
	// basis is what the speed and live progress count: TARGET when every
	// node counts bytes sent to the destination, SOURCE when none does
	// (older Agents count source reads), MIXED otherwise. nodeBasis holds
	// each node's, by node ID.
	basis     string
	nodeBasis map[string]string
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
	requests, nodeLimits := &nodecontrol.TaskRequestRates{}, map[string]nodecontrol.NodeRequestLimits{}
	nodeBasis := map[string]string{}
	for index, result := range results {
		if !result.valid {
			entry.valid = false
			break
		}
		nodeBasis[nodes[index].ID] = execution.BasisSource
		if result.rate.Basis == execution.BasisTarget {
			nodeBasis[nodes[index].ID] = execution.BasisTarget
		}
		entry.progressSupported = entry.progressSupported && result.rate.ProgressSupported
		for _, child := range result.rate.Progress {
			if len(progressReports) >= task.MaxProgressReports {
				progressTruncated = true
				break
			}
			progressReports = append(progressReports, task.ProgressReport{NodeID: nodes[index].ID, EnrollmentID: nodes[index].EnrollmentID, Child: child})
		}
		if reported := result.rate.Requests; requests != nil && reported != nil && validRequestRates(*reported) {
			requests.List += reported.Rates.List
			requests.Get += reported.Rates.Get
			requests.Head += reported.Rates.Head
			requests.Put += reported.Rates.Put
			nodeLimits[nodes[index].ID] = reported.NodeLimits
		} else {
			requests = nil
		}
		entry.total += result.rate.BytesPerSecond
		for child, rate := range result.rate.Children {
			entry.children[child] += rate
		}
	}
	if entry.valid && requests != nil {
		entry.requests, entry.nodeLimits = requests, nodeLimits
	}
	if entry.valid {
		entry.basis, entry.nodeBasis = combinedBasis(nodeBasis), nodeBasis
	}
	if !entry.valid && last.generation == p.Generation && last.progressSupported {
		// A failed collection cannot establish that an Agent lacks progress
		// support. Keep known capability so an incomplete sample never falls
		// back to a misleading completed-bytes-only average.
		entry.progressSupported = true
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

// combinedBasis is TARGET or SOURCE when every node's basis is that one,
// MIXED when they differ, and empty without nodes.
func combinedBasis(nodes map[string]string) string {
	combined := ""
	for _, basis := range nodes {
		if combined == "" {
			combined = basis
		} else if combined != basis {
			return "MIXED"
		}
	}
	return combined
}
func validRequestRates(r nodecontrol.TaskRequests) bool {
	for _, value := range []float64{r.Rates.List, r.Rates.Get, r.Rates.Head, r.Rates.Put, r.NodeLimits.List, r.NodeLimits.Head, r.NodeLimits.Put} {
		if !validTaskRate(value) {
			return false
		}
	}
	return true
}
func (t *taskRuntime) cachedRates(p task.Parent, maxAge time.Duration) (taskRateCache, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	entry, exists := t.rates[p.ID]
	return entry, exists && entry.valid && entry.generation == p.Generation && time.Since(entry.observed) <= maxAge
}
func (t *taskRuntime) rateSnapshot(p task.Parent) (taskRateCache, bool) {
	return t.cachedRates(p, taskRateMaxAge)
}
func (t *taskRuntime) rate(p task.Parent, child string) (float64, bool) {
	entry, fresh := t.rateSnapshot(p)
	if !fresh {
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
	displays.task(row, p.CurrentNodes())
	row["executionMembership"] = s.membershipSummary(p, displays)
	s.addTaskRate(row, p, nil)
	s.addLiveProgress(row, p, nil)
	s.addRequestRates(row, p)
	if activity := s.scanActivityView(p); activity != nil {
		row["scanActivity"] = activity
	}
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
	s.addTaskRate(row, p, &c)
	s.addLiveProgress(row, p, &c)
	return row
}

// addTaskRate writes the current speed and, with it, transferBasis: what the
// speed and live bytes count (see taskRateCache.basis). A child's basis is
// its node's.
func (s *Server) addTaskRate(row map[string]any, p task.Parent, child *task.Child) {
	row["currentSpeedBytesPerSecond"] = nil
	if task.Ended(p.Status) || task.Ended(stringValue(row["status"])) {
		row["currentSpeedBytesPerSecond"] = float64(0)
		return
	}
	entry, fresh := s.tasks.rateSnapshot(p)
	if !fresh {
		return
	}
	basis := entry.basis
	if child == nil {
		row["currentSpeedBytesPerSecond"] = entry.total
	} else {
		row["currentSpeedBytesPerSecond"] = entry.children[child.Job.ChildID]
		basis = entry.nodeBasis[child.Node.ID]
	}
	if basis != "" {
		row["transferBasis"] = basis
	}
}
func stringValue(value any) string { text, _ := value.(string); return text }
