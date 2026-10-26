package master

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/task"
)

func waitLeaseAuthorityEnds(t *testing.T, ctx context.Context, a *takeoverAgent, p task.Parent) {
	t.Helper()
	leaseContext, err := a.service.Leases.ParentContext(p.ID, p.Generation)
	if err == nil {
		select {
		case <-leaseContext.Done():
		case <-ctx.Done():
			t.Fatal("Agent lease remained active", ctx.Err())
		}
	}
}

func TestStopLeaseRequiresAnArmedExpiredOwner(t *testing.T) {
	f := newAgentHTTPFixture(t)
	a, b := takeoverTestAgent(t, f, "stop-a"), takeoverTestAgent(t, f, "stop-b")
	p := takeoverParent(t, f, a.node, b.node)
	runtime := f.master.tasks
	runtime.leases.ttl, runtime.leases.margin = time.Second, 50*time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := runtime.repo.EnableFailover(p.ID, p.Generation, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.leases.renew(ctx, p, a.node); err != nil {
		t.Fatal(err)
	}
	stopping, err := runtime.repo.Stop(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped, err := runtime.nodeLeaseStopped(stopping, a.node); err != nil || stopped {
		t.Fatal("unexpired owner falsely acknowledged a stop", stopped, err)
	}
	if stopped, err := runtime.nodeLeaseStopped(stopping, b.node); err != nil || stopped {
		t.Fatal("owner with no lease falsely acknowledged a stop", stopped, err)
	}
	a.cancel()
	short, endShort := context.WithTimeout(ctx, 30*time.Millisecond)
	err = runtime.stopMembers(short, stopping)
	endShort()
	if err == nil {
		t.Fatal("offline unexpired owner did not keep stop pending")
	}
	current, _ := runtime.repo.Get(p.ID)
	if current.Status != "STOPPING" {
		t.Fatal("early stop changed parent state", current.Status)
	}
	waitLeaseAuthorityEnds(t, ctx, a, p)
	for {
		stopped, err := runtime.nodeLeaseStopped(stopping, a.node)
		if err != nil {
			t.Fatal(err)
		}
		if stopped {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("Master did not honor the expired execution lease")
		case <-time.After(5 * time.Millisecond):
		}
	}
	// The generation remains registered on the online peer even when there is
	// no work. Give it an explicit durable stop so its ledger is available.
	if err := f.master.agentCall(ctx, nodeSession(b.node), "parent_stop", true, agentservice.ParentRequest{ParentID: p.ID, Generation: p.Generation}, nil); err != nil {
		t.Fatal(err)
	}
	fast, endFast := context.WithTimeout(ctx, 500*time.Millisecond)
	err = runtime.step(fast, p.ID)
	endFast()
	if err != nil {
		t.Fatal("expired offline scanner blocked stop reconciliation", err)
	}
	current, err = runtime.repo.Get(p.ID)
	if err != nil || current.Status != "STOPPED" || current.NodeRetired(a.node.ID) {
		t.Fatal("lease-based stop must preserve membership for later resume", current, err)
	}
	// A renewal already holding an old RUNNING snapshot may run after the stop.
	// It must consult the durable state, rather than reactivate the dead owner.
	if err := runtime.leases.renew(ctx, p, b.node); err == nil {
		t.Fatal("stale RUNNING snapshot renewed a stopped task")
	}
	if _, err := b.service.Engine.Accept(ctx, execution.Job{ParentID: p.ID, Number: 1, ChildID: "1-1", Generation: p.Generation, ConflictPolicy: "SKIP"}); !errors.Is(err, execution.ErrStopped) {
		t.Fatal("stopped task did not retain its durable admission fence", err)
	}
}

func TestStopLeaseNeverSubstitutesForLegacyNodeAcknowledgement(t *testing.T) {
	f := newAgentHTTPFixture(t)
	p := takeoverParent(t, f, task.Node{ID: "old", EnrollmentID: "old"}, task.Node{ID: "other", EnrollmentID: "other"})
	p, err := f.master.tasks.repo.Stop(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped, err := f.master.tasks.nodeLeaseStopped(p, p.Spec.Nodes[0]); err != nil || stopped {
		t.Fatal("legacy owner's stop was inferred without a lease", stopped, err)
	}
}

func TestStopAndResumeWithRetiredCompletedScannerPreservesTransferredBytes(t *testing.T) {
	f := newAgentHTTPFixture(t)
	storage := &takeoverStorage{source: map[string][]byte{}, target: map[string][]byte{}, reads: map[string]int{}, writes: map[string]int{},
		readStarted: make(chan struct{}), readCancelled: make(chan struct{}), listStarted: make(chan struct{}), listCancelled: make(chan struct{})}
	for i := 0; i < 8; i++ {
		storage.source[fmt.Sprintf("objects/%03d", i)] = bytes.Repeat([]byte(fmt.Sprintf("resume-%03d", i)), 16)
	}
	s3 := httptest.NewServer(storage)
	t.Cleanup(s3.Close)
	a, b := takeoverTestAgent(t, f, "resume-data-a"), takeoverTestAgent(t, f, "resume-data-b")
	labelTakeoverAgent(a, "old")
	labelTakeoverAgent(b, "new")
	if err := a.service.Engine.SetConcurrency(1, 1); err != nil {
		t.Fatal(err)
	}
	code, body := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/s3-config", f.admin, map[string]any{
		"alias": "qa", "endpoint": s3.URL, "accessKey": "fixture-ak", "secretKey": "fixture-sk", "region": "test", "pathStyleEnable": true, "validate": false,
	})
	if code != http.StatusOK {
		t.Fatal("storage setup", code, body)
	}
	p := takeoverParent(t, f, a.node, b.node)
	runtime := f.master.tasks
	runtime.leases.ttl, runtime.leases.margin = time.Second, 100*time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	startTakeoverLeaseLoop(t, ctx, runtime)
	for {
		lastErr := runtime.step(ctx, p.ID)
		current, err := runtime.repo.Get(p.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Counters.Succeeded == 1 && current.ScanComplete && current.DispatchedThrough == current.ScannedBatches {
			p = current
			break
		}
		if ctx.Err() != nil {
			t.Fatal("original scanner did not finish ahead of transfer", current, lastErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	a.cancel()
	waitLeaseAuthorityEnds(t, ctx, a, p)
	var err error
	for !p.NodeRetired(a.node.ID) {
		p, err = runtime.reconcileFailover(ctx, p, map[string]bool{b.node.ID: true})
		if err != nil {
			t.Fatal(err)
		}
		if ctx.Err() != nil {
			t.Fatal("expired scanner owner was not retired")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if p.ScannerNode().ID != a.node.ID || p.Failover.ScannerSeeding || !p.ScanComplete {
		t.Fatal("completed manifest unnecessarily started a replacement scanner", p)
	}
	if _, err := runtime.repo.Stop(p.ID); err != nil {
		t.Fatal(err)
	}
	fast, endFast := context.WithTimeout(ctx, 500*time.Millisecond)
	err = runtime.step(fast, p.ID)
	endFast()
	if err != nil {
		t.Fatal("stop waited for the retired scanner", err)
	}
	p, err = runtime.repo.Get(p.ID)
	if err != nil || p.Status != "STOPPED" || p.Counters.Succeeded != 1 {
		t.Fatal("stop lost existing transfer credit", p, err)
	}
	p, err = runtime.repo.Resume(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	for p.Status != "COMPLETED" {
		lastErr := runtime.step(ctx, p.ID)
		p, err = runtime.repo.Get(p.ID)
		if err != nil {
			t.Fatal(err)
		}
		if ctx.Err() != nil || p.Status == "FAILED" || p.Status == "PARTIAL_FAILED" {
			t.Fatal("resume waited for the retired scanner or could not restart its seed", p, lastErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if p.Counters.Succeeded != 8 || p.Counters.Skipped != 0 || p.Counters.Failed != 0 || p.ScannerNode().ID != b.node.ID || p.Generation != 2 || p.ScannerEpoch() != 1 {
		t.Fatal("resume counters or scanner ownership", p)
	}
	storage.mu.Lock()
	defer storage.mu.Unlock()
	if len(storage.target) != 8 || storage.reads["objects/000"] != 1 || storage.writes["objects/000"] != 1 {
		t.Fatal("resume lost data or retransmitted acknowledged data", len(storage.target), storage.reads["objects/000"], storage.writes["objects/000"])
	}
	for key, payload := range storage.source {
		if sha256.Sum256(payload) != sha256.Sum256(storage.target[key]) {
			t.Fatal("resumed bytes differ", key)
		}
	}
}
