package legacyimport

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/task"
)

func taskFixture(t *testing.T) *preparationFixture {
	f := prepareFixture(t)
	delete(f.master, "task:10")
	delete(f.master, "completed:10:a")
	req := map[string]any{"sourceS3Alias": "storage", "destS3Alias": "storage", "sourceBucket": "source", "destBucket": "destination", "conflictPolicy": "OVERWRITE", "distributionMode": "balanced", "prefix": "logs/", "taskBandwidthLimitMbps": 100.0, "taskBandwidthScopeId": "creation:fixture"}
	f.master["task_creation:fixture"] = map[string]any{"id": "fixture", "publicTaskId": 224, "taskIds": []int{224, 225}, "request": req, "scanComplete": true, "status": "COMPLETED", "createdAt": int64(1700000000000), "participatingWorkerNodes": []string{"master", "node"}}
	f.master["task:id:sequence"] = 1000
	for i, entry := range []struct {
		node   string
		values map[string]any
	}{{"master", f.master}, {"node", f.node}} {
		id := 224 + i
		entry.values["task:"+stringID(id)] = map[string]any{"taskIndex": id, "direction": "OSS_TO_OSS", "status": "COMPLETED", "scanComplete": true, "shardIndex": i, "shardId": "oss-fixture-" + stringID(i), "createTime": int64(1700000000000), "listObjectCount": 1, "listObjectSize": 3, "putObjectCount": 1, "putObjectSize": 3}
		key := base64.RawURLEncoding.EncodeToString([]byte("logs/" + stringID(i)))
		entry.values["file-report:"+stringID(id)+":item:"+key] = map[string]any{"taskId": id, "reportTaskId": id, "logicalKey": "logs/" + stringID(i), "result": "SUCCESS", "size": 3}
		entry.values["file-report:"+stringID(id)+":coverage"] = map[string]any{"ledgerCoverage": "TRACKED", "scanComplete": true}
	}
	f.master["task_bandwidth:desired:creation:fixture"] = map[string]any{"scopeId": "creation:fixture", "limitMbps": 100.0, "members": []string{"master", "node"}, "revision": 4}
	f.master["task:purged:800"] = true
	f.master["report:purged:800"] = true
	f.write(t)
	return f
}
func stringID(value int) string { return fmtInt(int64(value)) }
func fmtInt(value int64) string { return strconv.FormatInt(value, 10) }

func TestConvertTasksAggregatesCreationPreservesResultsAndFences(t *testing.T) {
	f := taskFixture(t)
	p, err := Prepare(context.Background(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	result, err := p.ConvertTasks(context.Background(), TaskImportOptions{LegacyTimezone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Parents != 2 || result.Children != 2 || result.ReadyToRun || !result.CatalogueComplete || result.HighWater != 1000 {
		t.Fatalf("bad conversion %+v", result)
	}
	repo := task.New(p.Master)
	parent, err := repo.Get(224)
	if err != nil {
		t.Fatal(err)
	}
	if parent.Status != "COMPLETED" || parent.Counters.Succeeded != 2 || parent.Spec.BandwidthMbps != 100 || parent.Spec.DistributionMode != "BALANCED" || len(parent.Spec.Nodes) != 2 {
		t.Fatalf("incorrect parent %+v", parent)
	}
	if _, err := repo.Get(225); !errors.Is(err, task.ErrNotFound) {
		t.Fatal("execution range became a second public task", err)
	}
	rows, err := repo.LegacyHistory(224, "OBJECT", "", 10)
	if err != nil || len(rows) != 2 {
		t.Fatal(rows, err)
	}
	children := map[string]bool{}
	for _, row := range rows {
		children[row.ChildTaskID] = true
	}
	if !children["224-1"] || !children["224-2"] {
		t.Fatal(children)
	}
	jobs, err := repo.Children(224, 0, 256)
	if err != nil || len(jobs) != 0 {
		t.Fatal("historical successes fabricated execution jobs", jobs, err)
	}
	repeated, err := p.ConvertTasks(context.Background(), TaskImportOptions{LegacyTimezone: "UTC"})
	if err != nil || repeated.HistoryRecords != result.HistoryRecords {
		t.Fatal(repeated, err)
	}
	if err := p.Master.ClaimRole("master"); !errors.Is(err, state.ErrMigrationPending) {
		t.Fatal("migration became runnable", err)
	}
	if err := p.Finalize(context.Background()); err == nil {
		t.Fatal("incomplete recovery published")
	}
}
func TestConvertTasksScanCompletionDoesNotMeanTransferCompletion(t *testing.T) {
	f := taskFixture(t)
	f.node["task:225"].(map[string]any)["status"] = "RUNNING"
	f.node["task:225"].(map[string]any)["putObjectCount"] = 0
	f.node["task:225"].(map[string]any)["marker"] = "middle"
	f.write(t)
	p, err := Prepare(context.Background(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := p.ConvertTasks(context.Background(), TaskImportOptions{}); err != nil {
		t.Fatal(err)
	}
	repo := task.New(p.Master)
	parent, err := repo.Get(224)
	if err != nil || parent.Status != "STOPPED" || !parent.Legacy.NeedsReconciliation || parent.Counters.Succeeded != 1 {
		t.Fatal(parent, err)
	}
	if _, err := repo.Resume(224); !errors.Is(err, task.ErrLegacyRecovery) {
		t.Fatal("legacy resumed without source/target reconciliation", err)
	}
	raw, err := p.sourceRecord("node", "task:225")
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if json.Unmarshal(raw.Value, &value) != nil || value["marker"] != "middle" {
		t.Fatal("checkpoint lost")
	}
}
func TestRealV4TaskSnapshotConversion(t *testing.T) {
	source := os.Getenv("OSS_TRANSFER_LEGACY_TASK_SNAPSHOT")
	if source == "" {
		t.Skip("set OSS_TRANSFER_LEGACY_TASK_SNAPSHOT to a stopped real V4 capture")
	}
	input, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Inspect(context.Background(), input)
	input.Close()
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "destination")
	p, err := Prepare(context.Background(), PrepareOptions{SnapshotPaths: []string{source}, Destination: destination, PublicURL: "http://127.0.0.1:1685", DirectNodes: map[string]DirectNodeApproval{plan.Header.Node: {Address: plan.Header.Node, Label: "Migrated fixture"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	result, err := p.ConvertTasks(context.Background(), TaskImportOptions{LegacyTimezone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	repo := task.New(p.Master)
	completed, stopped, recycled := 0, 0, 0
	rows, err := repo.List(0, 100, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Status == "COMPLETED" {
			completed++
			if row.Counters.Succeeded != 260 {
				t.Fatalf("historical completed count changed for %d: %d", row.ID, row.Counters.Succeeded)
			}
		} else if row.Status == "STOPPED" {
			stopped++
			if _, err := repo.Resume(row.ID); !errors.Is(err, task.ErrLegacyRecovery) {
				t.Fatal("unverified resume", err)
			}
		} else {
			t.Fatalf("unexpected historical state %d %s", row.ID, row.Status)
		}
		if row.Deleted {
			recycled++
		}
		t.Logf("parent=%d status=%s success=%d children=%d recycled=%t", row.ID, row.Status, row.Counters.Succeeded, row.ChildCount, row.Deleted)
	}
	if result.Parents != 6 || completed != 4 || stopped != 2 || recycled != 1 {
		t.Fatalf("capture mismatch: %+v completed=%d stopped=%d recycled=%d", result, completed, stopped, recycled)
	}
	t.Logf("real V4 catalogue: %d parents, %d children, %d historical records; startup remains fenced", result.Parents, result.Children, result.HistoryRecords)
}

func TestPartialWorkerPurgeDoesNotEraseOtherChildrenOrMarkParentCompleted(t *testing.T) {
	f := taskFixture(t)
	f.node["task:purged:225"] = true
	f.write(t)
	p, err := Prepare(context.Background(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := p.ConvertTasks(context.Background(), TaskImportOptions{}); err != nil {
		t.Fatal(err)
	}
	parent, err := task.New(p.Master).Get(224)
	if err != nil || parent.Purged || parent.Status != "STOPPED" {
		t.Fatal("one inconsistent worker purge erased the entire logical task", parent, err)
	}
	found := false
	for _, code := range parent.Legacy.Blockers {
		found = found || code == "EXECUTION_PURGE_FENCE_REQUIRES_REVIEW"
	}
	if !found {
		t.Fatal("lost inconsistent purge evidence")
	}
}

func TestRealV4RecoveryPreparationIsOfflineAndFenced(t *testing.T) {
	source := os.Getenv("OSS_TRANSFER_LEGACY_TASK_SNAPSHOT")
	if source == "" {
		t.Skip("real V4 snapshot required")
	}
	input, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Inspect(context.Background(), input)
	input.Close()
	if err != nil {
		t.Fatal(err)
	}
	p, err := Prepare(context.Background(), PrepareOptions{SnapshotPaths: []string{source}, Destination: filepath.Join(t.TempDir(), "destination"), PublicURL: "http://127.0.0.1:1685", DirectNodes: map[string]DirectNodeApproval{plan.Header.Node: {Address: plan.Header.Node}}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := p.ConvertTasks(context.Background(), TaskImportOptions{LegacyTimezone: "UTC"}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		prepared, err := p.PrepareRecovery(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if prepared.Parents != 2 || prepared.Keys < 388 {
			t.Fatal(prepared)
		}
	}
	repo := task.New(p.Master)
	for _, id := range []int64{7, 8} {
		parent, err := repo.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("parent=%d recoveryReady=%t blockers=%v", id, parent.Legacy.RecoveryReady, parent.Legacy.Blockers)
		if parent.Status != "STOPPED" || !parent.Legacy.NeedsReconciliation {
			t.Fatal("preparation resumed task")
		}
		if err := p.Master.ClaimRole("master"); !errors.Is(err, state.ErrMigrationPending) {
			t.Fatal("preparation released startup guard", err)
		}
	}
}
