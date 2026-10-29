package master

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/task"
)

func serveCleanupAgent(t *testing.T, f *agentHTTPFixture, node task.Node, respond func(context.Context, agenttransport.Command) agenttransport.Response) {
	t.Helper()
	broker := f.master.agents.broker
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			command, err := broker.Poll(ctx, nodeSession(node))
			if err != nil {
				return
			}
			if err := broker.Confirm(nodeSession(node), agenttransport.Receipt{RequestID: command.RequestID, DeliveryID: command.DeliveryID}); err != nil {
				continue
			}
			response := respond(ctx, command)
			_ = broker.Complete(nodeSession(node), agenttransport.Result{RequestID: command.RequestID, DeliveryID: command.DeliveryID, Response: response})
		}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("cleanup test Agent did not stop")
		}
	})
}

func waitCleanup(t *testing.T, f *agentHTTPFixture, id int64, check func(task.Parent) bool) task.Parent {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		p, err := f.master.tasks.repo.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if check(p) {
			return p
		}
		if time.Now().After(deadline) {
			t.Fatalf("cleanup did not reach expected state: %+v", p.Cleanup)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCleanupAdmissionDoesNotWaitForAgentAndDuplicatesAreIgnored(t *testing.T) {
	f := newAgentHTTPFixture(t)
	code, session := f.join(t, f.invite(t, "node.example"), "slow-cleanup", agentCredential(t))
	if code != 200 {
		t.Fatal(code)
	}
	p := purgeFixtureTask(t, f, session, "slow-cleanup")
	stopAndRecyclePurgeFixture(t, f, p)
	started, release := make(chan struct{}, 1), make(chan struct{})
	serveCleanupAgent(t, f, p.Spec.Nodes[0], func(ctx context.Context, command agenttransport.Command) agenttransport.Response {
		if command.Operation != "parent_cleanup" {
			t.Errorf("unexpected cleanup operation: %s", command.Operation)
		}
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return agenttransport.Response{Status: 200, Payload: json.RawMessage(`{"done":true,"recordsDeleted":12}`)}
	})
	f.master.Start()
	before := time.Now()
	code, body := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/permanent-delete", f.admin, map[string]any{"taskIds": []int64{p.ID}})
	if code != 200 || body["accepted"] != float64(1) || time.Since(before) > time.Second {
		t.Fatal("delete waited for Agent cleanup", code, body, time.Since(before))
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("background cleanup never started")
	}
	code, body = request(t, f.http.Client(), "POST", f.http.URL+"/api/master/permanent-delete", f.admin, map[string]any{"taskIds": []int64{p.ID}})
	if code != 200 || body["ignored"] != float64(1) || body["accepted"] != float64(0) {
		t.Fatal("duplicate cleanup was admitted again", code, body)
	}
	code, body = request(t, f.http.Client(), "POST", f.http.URL+"/api/master/cleanup-status", f.admin, map[string]any{"taskIds": []int64{p.ID}})
	row := body["tasks"].([]any)[0].(map[string]any)
	if code != 200 || row["purged"] != false || row["cleanup"] == nil {
		t.Fatal("pending cleanup disappeared", code, body)
	}
	if _, err := f.master.controlTask(context.Background(), p.ID, "restore"); !errors.Is(err, task.ErrState) {
		t.Fatal("cleanup task could be restored", err)
	}
	// Ordinary API reads remain available while the Agent is deliberately held.
	if code, _ := request(t, f.http.Client(), "GET", f.http.URL+"/api/master/status", f.admin, nil); code != 200 {
		t.Fatal("task list blocked by cleanup", code)
	}
	close(release)
	p = waitCleanup(t, f, p.ID, func(p task.Parent) bool { return p.Purged })
	if p.Cleanup.RecordsDeleted != 12 || p.Cleanup.Status != "DONE" {
		t.Fatal("final cleanup progress missing", p.Cleanup)
	}
}

func TestCleanupQueueSurvivesMasterRestartAndOfflineNodeDoesNotBlockOtherNode(t *testing.T) {
	f := newAgentHTTPFixture(t)
	_, a := f.join(t, f.invite(t, "a.example"), "cleanup-a", agentCredential(t))
	_, b := f.join(t, f.invite(t, "b.example"), "cleanup-b", agentCredential(t))
	p := takeoverParent(t, f, task.Node{ID: a.NodeID, EnrollmentID: a.EnrollmentID, Address: "a.example"}, task.Node{ID: b.NodeID, EnrollmentID: b.EnrollmentID, Address: "b.example"})
	stopAndRecyclePurgeFixture(t, f, p)
	if _, admitted, err := f.master.requestTaskPurge(context.Background(), p.ID); err != nil || !admitted {
		t.Fatal("cleanup not admitted", admitted, err)
	}
	// Restart before any node has acknowledged, without resubmitting deletion.
	f.close()
	f.open(t)
	f.master.tasks.cleanup.nodeTimeout = 50 * time.Millisecond
	var calls atomic.Int32
	serveCleanupAgent(t, f, p.Spec.Nodes[1], func(_ context.Context, command agenttransport.Command) agenttransport.Response {
		calls.Add(1)
		return agenttransport.Response{Status: 200, Payload: json.RawMessage(`{"done":true,"recordsDeleted":42}`)}
	})
	f.master.Start()
	waitCleanup(t, f, p.ID, func(p task.Parent) bool {
		return p.Cleanup != nil && p.Cleanup.Status == "WAITING" && p.Cleanup.Nodes[b.NodeID].Done
	})
	if calls.Load() != 1 {
		t.Fatal("completed node was redundantly polled", calls.Load())
	}
	// Restart again after only one node acknowledged: that acknowledgement
	// must survive, so the finished node need not even reconnect.
	f.close()
	f.open(t)
	f.master.tasks.cleanup.nodeTimeout = 50 * time.Millisecond
	f.master.Start()
	// The previously offline node reconnects. The durable operation finishes
	// automatically; there is no second permanent-delete request.
	serveCleanupAgent(t, f, p.Spec.Nodes[0], func(_ context.Context, command agenttransport.Command) agenttransport.Response {
		return agenttransport.Response{Status: 200, Payload: json.RawMessage(`{"done":true,"recordsDeleted":17}`)}
	})
	p = waitCleanup(t, f, p.ID, func(p task.Parent) bool { return p.Purged })
	if p.Cleanup.CompletedNodes != 2 || p.Cleanup.RecordsDeleted != 59 {
		t.Fatal("restart lost cleanup acknowledgements", p.Cleanup)
	}
	if calls.Load() != 1 {
		t.Fatal("completed node was polled after acknowledgement", calls.Load())
	}
}

func TestCleanupWorksWithRealAgentHTTPJournalAndLeavesFilesAndNeighborTask(t *testing.T) {
	f := newAgentHTTPFixture(t)
	agent := nodeTestAgent(t, f, "cleanup-real")
	members, err := f.master.agents.registry.Members()
	if err != nil || len(members) != 1 {
		t.Fatal(members, err)
	}
	p := purgeFixtureTask(t, f, members[0].Session(), "real-cleanup")
	keep := purgeFixtureTask(t, f, members[0].Session(), "keep-records")
	source, target := t.TempDir(), t.TempDir()
	for _, root := range []string{source, target} {
		if err := os.WriteFile(filepath.Join(root, "retained"), []byte("unchanged transfer data"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 300; i++ {
		job := execution.Job{ChildID: fmt.Sprintf("%d-%d", p.ID, i), ParentID: p.ID, Number: i, Generation: p.Generation,
			Source: execution.Endpoint{Kind: "LOCAL", Root: source}, Destination: execution.Endpoint{Kind: "LOCAL", Root: target}, ConflictPolicy: "SKIP", DiscoveryComplete: true}
		if _, err := agent.service.Jobs.Accept(context.Background(), job); err != nil {
			t.Fatal(err)
		}
	}
	neighbor := execution.Job{ChildID: fmt.Sprintf("%d-1", keep.ID), ParentID: keep.ID, Number: 1, Generation: keep.Generation,
		Source: execution.Endpoint{Kind: "LOCAL", Root: source}, Destination: execution.Endpoint{Kind: "LOCAL", Root: target}, ConflictPolicy: "SKIP", DiscoveryComplete: true}
	if _, err := agent.service.Jobs.Accept(context.Background(), neighbor); err != nil {
		t.Fatal(err)
	}
	stopAndRecyclePurgeFixture(t, f, p)
	stopAndRecyclePurgeFixture(t, f, keep)
	if _, err := f.master.tasks.repo.Recycle(keep.ID, false); err != nil {
		t.Fatal(err)
	}
	before, err := f.master.tasks.repo.Get(keep.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.master.Start()
	code, body := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/permanent-delete", f.admin, map[string]any{"taskIds": []int64{p.ID}})
	if code != 200 || body["accepted"] != float64(1) {
		t.Fatal(code, body)
	}
	waitCleanup(t, f, p.ID, func(p task.Parent) bool { return p.Purged })
	if jobs, err := agent.service.Jobs.List(context.Background(), p.ID, "", 1); err != nil || len(jobs) != 0 {
		t.Fatal("Agent ledger remains", len(jobs), err)
	}
	if jobs, err := agent.service.Jobs.List(context.Background(), keep.ID, "", 1); err != nil || len(jobs) != 1 {
		t.Fatal("neighbor Agent ledger changed", len(jobs), err)
	}
	after, err := f.master.tasks.repo.Get(keep.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("neighbor Master task changed", err)
	}
	for _, root := range []string{source, target} {
		data, err := os.ReadFile(filepath.Join(root, "retained"))
		if err != nil || string(data) != "unchanged transfer data" {
			t.Fatal("cleanup modified transferred files", err)
		}
	}
}

func TestCleanupStatusRequiresAdminAndDistinguishesMissingFromPurged(t *testing.T) {
	f := newAgentHTTPFixture(t)
	_, session := f.join(t, f.invite(t, "node.example"), "status-auth", agentCredential(t))
	p := purgeFixtureTask(t, f, session, "status-auth")
	for _, token := range []string{"", "not-an-admin"} {
		if code, _ := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/cleanup-status", token, map[string]any{"taskIds": []int64{p.ID}}); code != http.StatusUnauthorized {
			t.Fatal("anonymous cleanup status accepted", code)
		}
	}
	code, body := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/cleanup-status", f.admin, map[string]any{"taskIds": []int64{p.ID, 9999}})
	if code != 200 {
		t.Fatal(code, body)
	}
	for _, v := range body["tasks"].([]any) {
		row := v.(map[string]any)
		if row["missing"] != true || row["purged"] != false {
			t.Fatal("missing task reported as deleted", row)
		}
	}
	if code, _ := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/cleanup-status", f.admin, map[string]any{"taskIds": []int64{p.ID, p.ID}}); code != 400 {
		t.Fatal("duplicate status IDs accepted", code)
	}
}

func TestCleanupWaitingTasksDoNotStarveLaterReadyTask(t *testing.T) {
	f := newAgentHTTPFixture(t)
	_, offline := f.join(t, f.invite(t, "offline.example"), "fairness-offline", agentCredential(t))
	_, online := f.join(t, f.invite(t, "online.example"), "fairness-online", agentCredential(t))
	var waiting []task.Parent
	for i := range 2 {
		p := purgeFixtureTask(t, f, offline, fmt.Sprintf("blocked-%d", i))
		stopAndRecyclePurgeFixture(t, f, p)
		if _, _, err := f.master.requestTaskPurge(context.Background(), p.ID); err != nil {
			t.Fatal(err)
		}
		waiting = append(waiting, p)
	}
	ready := purgeFixtureTask(t, f, online, "ready-after-blocked")
	stopAndRecyclePurgeFixture(t, f, ready)
	if _, _, err := f.master.requestTaskPurge(context.Background(), ready.ID); err != nil {
		t.Fatal(err)
	}
	serveCleanupAgent(t, f, ready.Spec.Nodes[0], func(_ context.Context, _ agenttransport.Command) agenttransport.Response {
		return agenttransport.Response{Status: 200, Payload: json.RawMessage(`{"done":true,"recordsDeleted":1}`)}
	})
	f.master.tasks.cleanup.nodeTimeout = 20 * time.Millisecond
	f.master.Start()
	waitCleanup(t, f, ready.ID, func(p task.Parent) bool { return p.Purged })
	for _, p := range waiting {
		retained, err := f.master.tasks.repo.Get(p.ID)
		if err != nil || retained.Purged || retained.Cleanup == nil {
			t.Fatal("offline task falsely completed", retained, err)
		}
	}
}

func TestCleanupOlderAgentRetriesInBackgroundWithoutNewDeleteRequest(t *testing.T) {
	f := newAgentHTTPFixture(t)
	_, session := f.join(t, f.invite(t, "old.example"), "cleanup-old-agent", agentCredential(t))
	p := purgeFixtureTask(t, f, session, "old-agent")
	stopAndRecyclePurgeFixture(t, f, p)
	var attempts atomic.Int32
	serveCleanupAgent(t, f, p.Spec.Nodes[0], func(_ context.Context, command agenttransport.Command) agenttransport.Response {
		switch command.Operation {
		case "parent_cleanup":
			return agenttransport.Response{Status: http.StatusNotFound, Payload: json.RawMessage(`{"error":"unsupported operation"}`)}
		case "parent_delete":
			if attempts.Add(1) == 1 {
				return agenttransport.Response{Status: http.StatusGatewayTimeout, Payload: json.RawMessage(`{"error":"cleanup still running"}`)}
			}
			return agenttransport.Response{Status: http.StatusOK}
		default:
			t.Errorf("unexpected cleanup operation: %s", command.Operation)
			return agenttransport.Response{Status: http.StatusBadRequest}
		}
	})
	f.master.Start()
	code, body := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/permanent-delete", f.admin, map[string]any{"taskIds": []int64{p.ID}})
	if code != 200 || body["accepted"] != float64(1) {
		t.Fatal(code, body)
	}
	waitCleanup(t, f, p.ID, func(p task.Parent) bool { return p.Cleanup != nil && p.Cleanup.Status == "WAITING" })
	waitCleanup(t, f, p.ID, func(p task.Parent) bool { return p.Purged })
	if attempts.Load() != 2 {
		t.Fatal("older Agent did not finish through automatic retry", attempts.Load())
	}
}

func TestCleanupRejectsLateAuxiliaryMetadataWrites(t *testing.T) {
	f := newAgentHTTPFixture(t)
	_, session := f.join(t, f.invite(t, "late.example"), "cleanup-late-writes", agentCredential(t))
	p := purgeFixtureTask(t, f, session, "late-writes")
	stopAndRecyclePurgeFixture(t, f, p)
	entry := &taskLeaseEntry{record: taskLeaseRecord{ParentID: p.ID, Node: p.Spec.Nodes[0], Generation: p.Generation}}
	if err := f.master.tasks.leases.save(entry); err != nil {
		t.Fatal("ordinary recycled task should retain lease history", err)
	}
	if _, _, err := f.master.requestTaskPurge(context.Background(), p.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.master.tasks.leases.save(entry); !errors.Is(err, task.ErrState) {
		t.Fatal("late lease write escaped cleanup fence", err)
	}
	err := f.master.store.Update(func(tx *state.Tx) error {
		if err := allowTaskMetadataWrite(tx, p.ID); err != nil {
			return err
		}
		return tx.Put("master-task-storage-proofs", fmt.Sprint(p.ID), map[string]bool{"late": true})
	})
	if !errors.Is(err, task.ErrState) {
		t.Fatal("late storage proof escaped cleanup fence", err)
	}
}
