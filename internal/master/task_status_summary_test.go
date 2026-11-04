package master

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/discovery"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/task"
)

// statusHeavyParent resembles a finished 16-node task with failover and
// scheduling state, a scan checkpoint and request limits.
func statusHeavyParent(id int64, status string) task.Parent {
	spec := task.Spec{Direction: "OSS_TO_OSS", Source: execution.Endpoint{Kind: "S3", Ref: "source", Bucket: "source-bucket"}, Destination: execution.Endpoint{Kind: "S3", Ref: "target", Bucket: "target-bucket"}, ConflictPolicy: "SKIP", DistributionMode: "BALANCED", Prefix: "data/", BandwidthMbps: 800, RequestLimits: &execution.RequestLimits{Scope: "PER_NODE", ListQPS: 10, HeadQPS: 20, PutQPS: 30}}
	scheduling := &task.SchedulingState{Nodes: map[string]task.PendingLoad{}}
	failover := &task.FailoverState{Enabled: true, NodeEpochs: map[string]uint64{}, RebalancedEpochs: map[string]uint64{}, Retired: map[string]task.RetiredNode{}}
	for n := range 16 {
		node := fmt.Sprintf("node-%02d-%s", n, strings.Repeat("x", 24))
		spec.Nodes = append(spec.Nodes, task.Node{ID: node, EnrollmentID: "enrollment-" + node, Address: fmt.Sprintf("10.0.%d.%d:9000", n, id%250)})
		scheduling.Nodes[node] = task.PendingLoad{Bytes: int64(n) << 30, Objects: int64(n) * 1000, Children: int64(n)}
		failover.NodeEpochs[node], failover.RebalancedEpochs[node] = uint64(n), uint64(n)
	}
	failover.Retired[spec.Nodes[15].ID] = task.RetiredNode{RetiredAt: time.Unix(1700000000, 0).UTC()}
	created := time.Unix(1700000000+id, 0).UTC()
	finished := created.Add(time.Hour)
	p := task.Parent{ID: id, Spec: spec, Generation: 2, BudgetRevision: 5, Status: status, CreatedAt: created, StartedAt: &created, FinishedAt: &finished,
		ActiveTime:     &execution.ActiveTime{ElapsedMillis: 3600000},
		ScanCheckpoint: &discovery.RecoveryCheckpoint{Sequence: 9, Version: 1, ThroughBatch: 400, Objects: 51200, Bytes: 1 << 36, Cursor: strings.Repeat("prefix/key/", 40)},
		ScannedObjects: 51200, ScannedBytes: 1 << 36, ScanComplete: true, ScannedBatches: 400, DispatchedThrough: 400, DispatchedObjects: 51200, DispatchedBytes: 1 << 36,
		ChildCount: 400, FinishedChildren: 400, Counters: execution.Counters{Total: 51200, TotalBytes: 1 << 36, Succeeded: 51100, SucceededBytes: 1<<36 - 100, Failed: 100, FailedBytes: 100},
		Fingerprint: strings.Repeat("f", 64), Scheduling: scheduling, Failover: failover, Error: "scan warning", RuntimeError: "runtime warning",
		ChildOrder: &task.ChildOrderState{Version: 1, All: &task.AllChildOrderState{Version: 1, Revision: 400, Ready: true}},
	}
	if status == "RUNNING" {
		p.FinishedAt = nil
	}
	return p
}

func seedStatusParents(t testing.TB, db *state.Store, repo *task.Repository, total int, status func(int64) string) {
	t.Helper()
	if err := db.Update(func(tx *state.Tx) error {
		for i := int64(1); i <= int64(total); i++ {
			if _, err := tx.NextID("master-tasks"); err != nil {
				return err
			}
			p := statusHeavyParent(i, status(i))
			if err := tx.Put("master-tasks", fmt.Sprintf("%020d", i), p); err != nil {
				return err
			}
			if err := tx.Put("master-task-visible", fmt.Sprintf("%020d", i), i); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Rows written behind the repository's back model an older Master; the
	// startup rebuild creates their summaries.
	if err := repo.EnsureListingSummaries(); err != nil {
		t.Fatal(err)
	}
}

func statusPoll(t testing.TB, s *Server, query string, summary bool) (map[string]any, int) {
	t.Helper()
	r := httptest.NewRequest("GET", "/api/master/status"+query, nil)
	if summary {
		r.Header.Set("X-Task-View", "summary")
	}
	w := httptest.NewRecorder()
	s.taskStatusHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status poll: HTTP %d %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body, w.Body.Len()
}

func newStatusServer(t testing.TB) *Server {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "master.db"))
	if err != nil {
		t.Fatal(err)
	}
	s, _, err := New(db, Options{DataDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(); _ = db.Close() })
	return s
}

// Rows and totals served from summaries must equal rows built from parents.
func TestTaskStatusSummaryViewMatchesFullParents(t *testing.T) {
	s := newStatusServer(t)
	statuses := []string{"COMPLETED", "FAILED", "PARTIAL_FAILED", "STOPPED", "RUNNING", "WAITING"}
	seedStatusParents(t, s.store, s.tasks.repo, 30, func(id int64) string { return statuses[id%int64(len(statuses))] })
	// Freeze wall-clock dependent fields (running durations) by comparing the
	// rendered rows directly with a fixed node display snapshot.
	full, err := s.tasks.repo.Recent(0, 100, false)
	if err != nil {
		t.Fatal(err)
	}
	summaries, err := s.tasks.repo.RecentSummaries(0, 100, false)
	if err != nil || len(summaries) != len(full) {
		t.Fatal(len(summaries), err)
	}
	displays := s.nodeDisplaySnapshot()
	for i := range full {
		want, got := s.taskView(full[i], displays), s.taskView(summaries[i], displays)
		for _, row := range []map[string]any{want, got} {
			delete(row, "duration")
			delete(row, "averageSpeedBytesPerSecond")
		}
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("task %d differs\nfull:    %#v\nsummary: %#v", full[i].ID, want, got)
		}
	}
	for _, query := range []string{"", "?limit=7", "?limit=7&beforeId=20", "?status=RUNNING", "?workerNode=10.0.3.4:9000", "?sourceBucket=source-bucket"} {
		fullBody, _ := statusPoll(t, s, query, false)
		summaryBody, _ := statusPoll(t, s, query, true)
		for _, body := range []map[string]any{fullBody, summaryBody} {
			for _, row := range body["tasks"].([]any) {
				delete(row.(map[string]any), "duration")
				delete(row.(map[string]any), "averageSpeedBytesPerSecond")
			}
		}
		if !reflect.DeepEqual(fullBody, summaryBody) {
			t.Fatalf("%q: summary response differs\nfull:    %v\nsummary: %v", query, fullBody, summaryBody)
		}
	}
}

// BenchmarkTaskStatusPoll measures one console poll (/api/master/status with
// the default page) over N finished historical tasks.
//
//	go test ./internal/master -run '^$' -bench TaskStatusPoll -benchmem
func BenchmarkTaskStatusPoll(b *testing.B) {
	for _, total := range []int{1000, 10000} {
		s := newStatusServer(b)
		seedStatusParents(b, s.store, s.tasks.repo, total, func(int64) string { return "COMPLETED" })
		for _, mode := range []string{"full", "summary"} {
			b.Run(fmt.Sprintf("tasks=%d/%s", total, mode), func(b *testing.B) {
				b.ReportAllocs()
				size := 0
				for range b.N {
					r := httptest.NewRequest("GET", "/api/master/status", nil)
					if mode == "summary" {
						r.Header.Set("X-Task-View", "summary")
					}
					w := httptest.NewRecorder()
					s.taskStatusHTTP(w, r)
					if w.Code != http.StatusOK {
						b.Fatal(w.Code)
					}
					size = w.Body.Len()
				}
				b.ReportMetric(float64(size)/1024, "responseKiB/op")
			})
		}
	}
}
