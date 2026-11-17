package master

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/discovery"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func saveParent(t *testing.T, f *agentHTTPFixture, p task.Parent) task.Parent {
	t.Helper()
	if err := f.db.Update(func(tx *state.Tx) error {
		return tx.Put("master-tasks", fmt.Sprintf("%020d", p.ID), p)
	}); err != nil {
		t.Fatal(err)
	}
	saved, err := f.master.tasks.repo.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

func scanningReply(p task.Parent, attempt string) agentservice.ScanReply {
	mean := 12.5
	return agentservice.ScanReply{State: discovery.State{Status: discovery.Scanning, Attempt: attempt, Generation: p.Generation},
		ScanProtocol: discovery.ScanProtocol, Activity: &transferstore.ScanActivitySnapshot{Mode: transferstore.ScanModeSorted,
			NamesRead: 86980, Examined: 1200, AlreadyFound: 3, Vanished: 1, LookupMeanMillis: &mean, OldestCallMillis: 6000, IdleMillis: 2000,
			Directory: &transferstore.ScanDirectorySnapshot{Key: "data/2026", Reading: false, Names: 86980, Position: 1201, Examined: 1200, ElapsedMillis: 300000}}}
}

// A running task with an incomplete scan shows what its scanner last
// reported: LIVE while the reply is fresh, STALE once it is older than the
// rates or the latest call failed, and UNSUPPORTED for a scanner without the
// scan protocol, never as zeros.
func TestScanActivityFollowsTheScannerReplies(t *testing.T) {
	f, p := progressMasterFixture(t, 2)
	clock := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	f.master.options.Now = func() time.Time { return clock }
	p.Status = "RUNNING"
	p = saveParent(t, f, p)
	runtime := f.master.tasks
	activity := func(p task.Parent) map[string]any {
		t.Helper()
		value, _ := f.master.taskView(p)["scanActivity"].(map[string]any)
		return value
	}
	if activity(p) != nil {
		t.Fatal("activity shown before any reply")
	}
	runtime.recordScan(p, agentservice.ScanReply{State: discovery.State{Status: discovery.Scanning, Attempt: "old"}}, nil)
	if got := activity(p); len(got) != 1 || got["state"] != scanActivityUnsupported {
		t.Fatalf("scanner without the protocol: %v", got)
	}
	runtime.recordScan(p, scanningReply(p, "run-1"), nil)
	got := activity(p)
	if got["state"] != scanActivityLive || got["namesRead"] != int64(86980) || got["examined"] != int64(1200) || got["alreadyFound"] != int64(3) ||
		got["vanished"] != int64(1) || got["lookupMeanMillis"] != 12.5 || got["oldestCallMillis"] != int64(6000) || got["ageMillis"] != int64(0) ||
		got["observedAt"] != clock.UnixMilli() || got["mode"] != transferstore.ScanModeSorted {
		t.Fatalf("live activity: %v", got)
	}
	if d, _ := got["directory"].(map[string]any); d["key"] != "data/2026" || d["position"] != int64(1201) || d["names"] != int64(86980) || d["examined"] != int64(1200) {
		t.Fatalf("directory: %v", got["directory"])
	}
	clock = clock.Add(scanActivityMaxAge + time.Second)
	if got := activity(p); got["state"] != scanActivityStale || got["namesRead"] != int64(86980) || got["ageMillis"] != int64(11000) {
		t.Fatalf("aged activity: %v", got)
	}
	runtime.recordScan(p, scanningReply(p, "run-1"), nil)
	runtime.recordScan(p, agentservice.ScanReply{}, errors.New("scanner unreachable"))
	if got := activity(p); got["state"] != scanActivityStale || got["examined"] != int64(1200) {
		t.Fatalf("activity after a failed call: %v", got)
	}
	// A source that reports no activity shows none.
	reply := scanningReply(p, "run-2")
	reply.Activity = nil
	runtime.recordScan(p, reply, nil)
	if got := activity(p); got != nil {
		t.Fatalf("activity shown for a source without any: %v", got)
	}
	runtime.recordScan(p, scanningReply(p, "run-2"), nil)
	for name, changed := range map[string]task.Parent{
		"another generation": func() task.Parent { c := p; c.Generation++; return c }(),
		"a complete scan":    func() task.Parent { c := p; c.ScanComplete = true; return c }(),
		"a stopped task":     func() task.Parent { c := p; c.Status = "STOPPED"; return c }(),
	} {
		if got := activity(changed); got != nil {
			t.Fatalf("activity shown for %s: %v", name, got)
		}
	}
	view, err := f.master.taskNodesView(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if scan := view["scan"].(map[string]any); scan["phase"] != scanPhaseScanning || scan["activity"] == nil || scan["upgradeRequired"] != nil {
		t.Fatalf("node view scan: %v", scan)
	}
}

// A scan checkpoint that requires the scan protocol goes only to a member that
// reports it. Until one does, the scan waits, and the node view says why.
func TestScanCheckpointWaitsForAMemberThatSpeaksItsProtocol(t *testing.T) {
	f, p := progressMasterFixture(t, 3)
	p.Status = "RUNNING"
	p.Failover = &task.FailoverState{Enabled: true, Retired: map[string]task.RetiredNode{"node-0": {RetiredAt: time.Now().UTC()}}}
	p.ScanCheckpoint = &discovery.RecoveryCheckpoint{Version: 1, Sequence: 1, Cursor: "saved", RequiresScanProtocol: 1}
	p.Shares = nil // The retired node's share is not what this test is about.
	p = saveParent(t, f, p)
	runtime := f.master.tasks
	protocols := map[string]int{"node-1": 0, "node-2": 0}
	var asked []string
	runtime.scanQuery = func(_ context.Context, node task.Node) (int, error) {
		asked = append(asked, node.ID)
		protocol, ok := protocols[node.ID]
		if !ok {
			return 0, errors.New("unreachable")
		}
		return protocol, nil
	}
	available := map[string]bool{"node-1": true, "node-2": true}
	ctx := context.Background()
	moved, err := runtime.moveRetiredScanner(ctx, p, available)
	if err != nil || moved.ScannerNode().ID != "node-0" || !runtime.waitsForScanUpgrade(moved) || scanPhase(moved) != scanPhaseWaiting {
		t.Fatalf("checkpoint handed to an older Agent: scanner %s, %v", moved.ScannerNode().ID, err)
	}
	view, err := f.master.taskNodesView(ctx, moved)
	if err != nil {
		t.Fatal(err)
	}
	if scan := view["scan"].(map[string]any); scan["phase"] != scanPhaseWaiting || scan["upgradeRequired"] != true {
		t.Fatalf("node view of the wait: %v", scan)
	}
	// An unreachable member is not known to be old.
	delete(protocols, "node-1")
	delete(protocols, "node-2")
	if moved, err = runtime.moveRetiredScanner(ctx, p, available); err != nil || runtime.waitsForScanUpgrade(moved) {
		t.Fatalf("unanswered queries reported as old Agents: %v", err)
	}
	protocols["node-1"], protocols["node-2"] = 0, discovery.ScanProtocol
	moved, err = runtime.moveRetiredScanner(ctx, p, available)
	if err != nil || moved.ScannerNode().ID != "node-2" || !moved.Failover.ScannerSeeding || runtime.waitsForScanUpgrade(moved) {
		t.Fatalf("upgraded member did not take over: scanner %s, %v", moved.ScannerNode().ID, err)
	}
	// A checkpoint without the requirement goes to the first member, unasked.
	asked = nil
	p.ScanCheckpoint.RequiresScanProtocol = 0
	p = saveParent(t, f, p)
	if moved, err = runtime.moveRetiredScanner(ctx, p, available); err != nil || moved.ScannerNode().ID != "node-1" || len(asked) != 0 {
		t.Fatalf("plain checkpoint: scanner %s, asked %v, %v", moved.ScannerNode().ID, asked, err)
	}
}
