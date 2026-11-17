package master

import (
	"context"
	"errors"
	"math"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/task"
)

func liveTask(status string) bool { return status == "RUNNING" || status == "STOPPING" }

// progressSnapshot is the sample that file progress reads: complete, or
// partial with the reporting nodes' children.
func (t *taskRuntime) progressSnapshot(p task.Parent) (taskRateCache, bool) {
	return t.sampleSnapshot(p, 3*time.Second)
}

// Live bytes are an observation, never durable success credit. Taking the
// larger of two complete totals avoids adding an old in-flight sample to a
// newer ledger that already includes that same object.
//
// transferredBytes is always set so clients need no fallback; inFlightBytes is
// nil when a live task's sample is missing or stale (unknown, not zero).
//
// liveProgress is LIVE for a complete sample. It is PARTIAL for a task whose
// nodes did not all report: transferredBytes then holds the reporting nodes'
// in-flight bytes only, a lower bound, and unreportedNodes names the others.
// A child on such a node is STALE and names its node.
func (s *Server) addLiveProgress(row map[string]any, p task.Parent, child *task.Child, displays nodeDisplaySet) {
	durable, _ := row["putObjectSize"].(int64)
	row["transferredBytes"], row["inFlightBytes"], row["liveProgress"] = durable, int64(0), "NONE"
	defer addProgressPercent(row)
	if !liveTask(p.Status) || !liveTask(stringValue(row["status"])) {
		return
	}
	row["inFlightBytes"], row["liveProgress"] = nil, "STALE"
	entry, fresh := s.tasks.sampleSnapshot(p, taskRateMaxAge)
	if fresh && !entry.progressSupported {
		// Older Agents report speed only. Keep their completed-object average.
		row["liveProgress"] = "UNSUPPORTED"
		return
	}
	if !entry.progressSupported {
		return
	}
	var unreported []task.Node
	if fresh && entry.partial {
		unreported = entry.missing
		if child != nil {
			unreported = nil
			if entry.missed(child.Node) {
				unreported = []task.Node{child.Node}
			}
		}
	}
	if len(unreported) > 0 {
		row["unreportedNodes"] = unreportedNodeViews(unreported, displays)
	}
	if !fresh || entry.progress == nil || child != nil && len(unreported) > 0 {
		// An unavailable live sample is unknown, not zero throughput.
		if p.ResumeTiming == nil {
			delete(row, "averageSpeedBytesPerSecond")
		}
		return
	}
	timing := p.ExecutionTime()
	bytes := max(durable, entry.progress.TotalBytes)
	if child != nil {
		if child.Record == nil {
			return
		}
		timing, bytes = child.Record.ExecutionTime(), durable
		for _, sample := range entry.progress.Children {
			if sample.Progress.ChildID == child.Job.ChildID && sample.Node.ID == child.Node.ID && sample.Node.EnrollmentID == child.Node.EnrollmentID && sample.Progress.Assignment == child.Job.Assignment && sample.Progress.Generation == child.Job.Generation && sample.Progress.Attempt >= child.Record.Attempt {
				bytes = max(durable, sample.Bytes)
				break
			}
		}
	}
	if len(unreported) > 0 {
		// The task's average needs every node's bytes.
		row["transferredBytes"], row["inFlightBytes"], row["liveProgress"] = bytes, bytes-durable, "PARTIAL"
		if p.ResumeTiming == nil {
			delete(row, "averageSpeedBytesPerSecond")
		}
		return
	}
	row["transferredBytes"], row["inFlightBytes"], row["liveProgress"] = bytes, bytes-durable, "LIVE"
	if child == nil {
		addInFlightObjects(row, entry.progress.Children)
	}
	if p.ResumeTiming != nil {
		return
	}
	if duration := timing.Duration(time.Now()); duration > 0 && !timing.Incomplete {
		row["averageSpeedBytesPerSecond"] = float64(bytes) / duration.Seconds()
		row["averageSpeedIncludesInFlight"] = true
	}
}

// addInFlightObjects adds the objects being transferred right now and the
// bytes they have transferred so far, from the same samples as the live bytes.
// A stop discards those bytes: when the task continues, these objects are
// transferred again from the start. Completed objects keep their results.
func addInFlightObjects(row map[string]any, children []task.ProgressChild) {
	objects, bytes := 0, int64(0)
	for _, child := range children {
		objects += child.Progress.ActiveObjects
		bytes += child.Progress.InFlightBytes
	}
	row["inFlightObjectCount"], row["inFlightObjectBytes"] = objects, bytes
}

// addProgressPercent computes the one percentage shown by the UI and CLI.
// Bytes are the basis whenever sizes are known, so a single large object in
// flight moves the bar; counts still gate 100% so pending zero-byte objects
// and a final multipart commit cannot look finished. Failed bytes stay out,
// as with counts. A sample that briefly overlaps a newly committed skip can
// count those bytes twice for about one sample; the caps keep that harmless.
func addProgressPercent(row map[string]any) {
	count := func(name string) int64 {
		switch value := row[name].(type) {
		case int64:
			return value
		case int:
			return int64(value)
		}
		return 0
	}
	total, totalBytes := count("listObjectCount"), count("listObjectSize")
	handled := count("putObjectCount") + count("skippedObjectCount")
	row["progressPercent"], row["progressBasis"] = nil, "COUNT"
	if totalBytes > 0 {
		row["progressBasis"] = "BYTES"
	}
	if stringValue(row["status"]) == "COMPLETED" {
		row["progressPercent"] = float64(100)
		return
	}
	// The denominator still grows during a scan, and handled > scanned means
	// the counters are mid-update; clients keep their indeterminate states.
	if complete, _ := row["scanComplete"].(bool); !complete || handled > total {
		return
	}
	percent := float64(0)
	if totalBytes > 0 {
		percent = float64(count("transferredBytes")+count("skippedObjectSize")) / float64(totalBytes) * 100
	} else if total > 0 {
		percent = float64(handled) / float64(total) * 100
	}
	percent = min(percent, 100)
	if handled < total {
		percent = min(percent, 99.9)
	}
	// Truncate so 99.996 never rounds up to 100 in a client.
	row["progressPercent"] = math.Floor(percent*100) / 100
}

// unreportedNodeViews names the nodes whose live progress is missing from a
// sample, as offlineTaskNodes names offline nodes.
func unreportedNodeViews(nodes []task.Node, displays nodeDisplaySet) []map[string]any {
	views := make([]map[string]any, 0, len(nodes))
	for _, node := range nodes {
		views = append(views, map[string]any{"executionNodeIp": node.Address, "displayLabel": displays.lookup(node.ID, node.Address).DisplayLabel})
	}
	return views
}

// validFileProgress drops an optional field an Agent reported out of range
// and keeps the row: an unknown phase or basis, or source reads outside
// [0, size]. A row without a basis reads as one counted from source reads.
func validFileProgress(file execution.ObjectProgress) execution.ObjectProgress {
	switch file.Phase {
	case "", execution.PhaseRetryWait, execution.PhaseRequestLimit, execution.PhaseUploadBuffer:
	default:
		file.Phase = ""
	}
	if file.ReadBytes < 0 || file.ReadBytes > file.Size {
		file.ReadBytes = 0
	}
	if file.Basis != execution.BasisTarget && file.Basis != execution.BasisSource {
		file.Basis = ""
	}
	return file
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
	// basisReported: each file carries the basis its Agent reported, so a
	// file without one is an older Agent's. Older Masters drop every basis.
	data := map[string]any{"parentTaskId": id, "generation": p.Generation, "supported": true, "partial": false, "items": items, "total": 0, "hasMore": false, "observedAt": time.Now().UnixMilli(), "basisReported": true}
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
	displays := s.nodeDisplaySnapshot()
	if entry.partial {
		// The reporting nodes' files are listed; the others are named. A
		// child's view depends on its own node only.
		unreported := entry.missing
		if number > 0 {
			unreported = nil
			if children, err := s.tasks.repo.Children(id, number-1, 1); err == nil && len(children) == 1 && children[0].Job.Number == number && entry.missed(children[0].Node) {
				unreported = []task.Node{children[0].Node}
			}
		}
		if len(unreported) > 0 {
			data["partial"], data["unreportedNodes"] = true, unreportedNodeViews(unreported, displays)
		}
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
				items = append(items, fileProgressView{validFileProgress(file), child.Progress.ChildID, child.Progress.Number, displays.lookup(child.Node.ID, child.Node.Address).DisplayLabel})
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
