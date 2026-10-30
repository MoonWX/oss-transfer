package master

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/task"
)

func liveTask(status string) bool { return status == "RUNNING" || status == "STOPPING" }

func (t *taskRuntime) progressSnapshot(p task.Parent) (taskRateCache, bool) {
	return t.cachedRates(p, 3*time.Second)
}

// Live bytes are an observation, never durable success credit. Taking the
// larger of two complete totals avoids adding an old in-flight sample to a
// newer ledger that already includes that same object.
func (s *Server) addLiveAverage(row map[string]any, p task.Parent, child *task.Child) {
	if !liveTask(p.Status) || !liveTask(stringValue(row["status"])) || p.ResumeTiming != nil {
		return
	}
	entry, fresh := s.tasks.rateSnapshot(p)
	if !entry.progressSupported {
		return
	}
	if !fresh || entry.progress == nil {
		// An unavailable live sample is unknown, not zero throughput.
		delete(row, "averageSpeedBytesPerSecond")
		return
	}
	timing := p.ExecutionTime()
	bytes := max(p.Counters.SucceededBytes, entry.progress.TotalBytes)
	if child != nil {
		if child.Record == nil {
			return
		}
		timing = child.Record.ExecutionTime()
		bytes = child.Record.Counters.SucceededBytes
		for _, sample := range entry.progress.Children {
			if sample.Progress.ChildID == child.Job.ChildID && sample.Node.ID == child.Node.ID && sample.Node.EnrollmentID == child.Node.EnrollmentID && sample.Progress.Assignment == child.Job.Assignment && sample.Progress.Generation == child.Job.Generation && sample.Progress.Attempt >= child.Record.Attempt {
				bytes = max(bytes, sample.Bytes)
				break
			}
		}
	}
	if duration := timing.Duration(time.Now()); duration > 0 && !timing.Incomplete {
		row["averageSpeedBytesPerSecond"] = float64(bytes) / duration.Seconds()
		row["averageSpeedIncludesInFlight"] = true
	}
}

type fileProgressReply struct {
	Supported bool                       `json:"supported"`
	Child     *execution.ChildProgress   `json:"child"`
	Items     []execution.ObjectProgress `json:"items"`
}

type fileProgressView struct {
	execution.ObjectProgress
	ChildTaskID        string `json:"childTaskId"`
	SubtaskNumber      int    `json:"subtaskNumber"`
	ExecutionNodeLabel string `json:"executionNodeLabel"`
}

// File names are fetched only for an open detail view. Background metrics carry
// small per-child totals; neither path writes progress to the task database.
func (s *Server) taskProgressHTTP(w http.ResponseWriter, r *http.Request) {
	id, ok := taskPathID(w, r)
	if !ok {
		return
	}
	p, err := s.tasks.repo.Get(id)
	if err != nil || p.Deleted || p.Purged {
		if errors.Is(err, task.ErrNotFound) || (err == nil && (p.Deleted || p.Purged)) {
			fail(w, 404, "Task not found")
		} else {
			fail(w, 500, "Cannot read task")
		}
		return
	}
	if raw := r.URL.Query().Get("generation"); raw != "" {
		generation, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || generation == 0 {
			fail(w, 400, "Invalid task generation")
			return
		}
		if generation != p.Generation {
			fail(w, 409, "Task generation changed")
			return
		}
	}
	number := 0
	if raw := r.URL.Query().Get("childNumber"); raw != "" {
		number, err = strconv.Atoi(raw)
		if err != nil || number < 1 || number > p.ChildCount {
			fail(w, 400, "Invalid child number")
			return
		}
	}
	items := []fileProgressView{}
	data := map[string]any{"parentTaskId": id, "generation": p.Generation, "supported": true, "partial": false, "items": items, "total": 0, "hasMore": false, "observedAt": time.Now().UnixMilli()}
	if number > 0 {
		data["childNumber"] = number
	}
	write := func() { writeJSON(w, 200, map[string]any{"success": true, "data": data}) }
	if !liveTask(p.Status) {
		write()
		return
	}
	s.tasks.sampleRates(r.Context(), p)
	entry, fresh := s.tasks.progressSnapshot(p)
	if !fresh {
		data["partial"] = true
		write()
		return
	}
	if !entry.progressSupported {
		data["supported"] = false
		write()
		return
	}
	if entry.progress == nil {
		data["partial"] = true
		write()
		return
	}
	limit := 20
	if number > 0 {
		limit = 128
	}
	children := append([]task.ProgressChild(nil), entry.progress.Children...)
	sort.Slice(children, func(i, j int) bool { return children[i].Progress.Number < children[j].Progress.Number })
	var selected []task.ProgressChild
	total, selectedCount := 0, 0
	for _, child := range children {
		if number > 0 && child.Progress.Number != number {
			continue
		}
		total += child.Progress.ActiveObjects
		if child.Progress.ActiveObjects > 0 && selectedCount < limit {
			selected = append(selected, child)
			selectedCount += child.Progress.ActiveObjects
		}
	}
	type result struct {
		reply fileProgressReply
		err   error
	}
	results := make([]result, len(selected))
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	var workers sync.WaitGroup
	slots := make(chan struct{}, 4)
	for index, child := range selected {
		workers.Add(1)
		go func() {
			defer workers.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				results[index].err = ctx.Err()
				return
			}
			results[index].err = s.agentCall(ctx, nodeSession(child.Node), "task_progress", false, map[string]any{"parentId": id, "generation": p.Generation, "childId": child.Progress.ChildID}, &results[index].reply)
		}()
	}
	workers.Wait()
	var reports []task.ProgressReport
	accepted := make([]bool, len(results))
	for index, result := range results {
		child := selected[index]
		total -= child.Progress.ActiveObjects
		reply := result.reply
		if result.err != nil || !reply.Supported || len(reply.Items) > 128 {
			data["partial"] = true
			continue
		}
		if reply.Child == nil { // The attempt finished while the view refreshed.
			continue
		}
		if reply.Child.ChildID != child.Progress.ChildID || reply.Child.Number != child.Progress.Number || reply.Child.Generation != p.Generation || reply.Child.Assignment != child.Progress.Assignment || reply.Child.Attempt != child.Progress.Attempt {
			data["partial"] = true
			continue
		}
		accepted[index] = true
		reports = append(reports, task.ProgressReport{NodeID: child.Node.ID, EnrollmentID: child.Node.EnrollmentID, Child: *reply.Child})
		total += len(reply.Items)
	}
	// A retry or reassignment may occur during the Agent call. Revalidate against
	// one current ledger snapshot before exposing the returned file names/bytes.
	projection, err := s.tasks.repo.ProjectProgress(id, p.Generation, reports)
	if errors.Is(err, task.ErrGeneration) {
		fail(w, 409, "Task generation changed")
		return
	}
	if err != nil {
		fail(w, 500, "Cannot read task")
		return
	}
	allowed := map[string]task.ProgressChild{}
	for _, child := range projection.Children {
		allowed[child.Progress.ChildID] = child
	}
	displays := s.nodeDisplaySnapshot()
	for index, result := range results {
		if !accepted[index] {
			continue
		}
		child, ok := allowed[result.reply.Child.ChildID]
		if !ok || child.Node.ID != selected[index].Node.ID || child.Progress.Attempt != result.reply.Child.Attempt || child.Progress.Assignment != result.reply.Child.Assignment {
			total -= len(result.reply.Items)
			data["partial"] = true
			continue
		}
		seen := map[string]bool{}
		for _, file := range result.reply.Items {
			if file.ItemID == "" || seen[file.ItemID] || file.Size < 0 || file.TransferredBytes < 0 || file.TransferredBytes > file.Size || (file.Stage != "TRANSFERRING" && file.Stage != "FINALIZING") {
				total--
				data["partial"] = true
				continue
			}
			seen[file.ItemID] = true
			if len(items) < limit {
				items = append(items, fileProgressView{file, child.Progress.ChildID, child.Progress.Number, displays.lookup(child.Node.ID, child.Node.Address).DisplayLabel})
			}
		}
	}
	if !liveTask(projection.Parent.Status) {
		items, total = []fileProgressView{}, 0
	}
	data["items"], data["total"], data["hasMore"] = items, max(total, len(items)), total > len(items)
	data["observedAt"] = time.Now().UnixMilli()
	write()
}
