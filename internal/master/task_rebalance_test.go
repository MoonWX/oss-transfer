package master

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

type handoffFaultExecutor struct {
	*agentservice.Service
	loseReply atomic.Bool
}

func (e *handoffFaultExecutor) Execute(ctx context.Context, c agenttransport.Command) (agenttransport.Response, error) {
	response, err := (takeoverExecutor{e.Service}).Execute(ctx, c)
	if c.Operation == "child_release_queued" && err == nil && response.Status == 200 {
		var ack struct {
			Released bool `json:"released"`
		}
		_ = json.Unmarshal(response.Payload, &ack)
		if ack.Released && e.loseReply.Swap(false) {
			return agenttransport.Response{Status: 503, Payload: json.RawMessage(`{"message":"lost release response"}`)}, nil
		}
	}
	return response, err
}
func handoffTestAgent(t *testing.T, f *agentHTTPFixture, name string) (*takeoverAgent, *handoffFaultExecutor) {
	t.Helper()
	credential := agentCredential(t)
	code, session := f.join(t, f.invite(t, name+".example"), name, credential)
	if code != 200 {
		t.Fatal(code)
	}
	db, err := state.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	service, err := agentservice.New(db, agentservice.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &takeoverAgent{node: task.Node{ID: session.NodeID, EnrollmentID: session.EnrollmentID, Address: name + ".example"}, service: service, cancel: cancel, done: make(chan error, 1)}
	executor := &handoffFaultExecutor{Service: service}
	runner := agenttransport.NewRunner(agentClient(t, f, credential), executor, nil, agenttransport.RunnerOptions{ReconnectDelay: time.Millisecond})
	go func() { a.done <- runner.Run(ctx) }()
	t.Cleanup(func() { a.cancel(); <-a.done; service.Close(context.Background()); db.Close() })
	return a, executor
}

// The scan is already complete before the node returns. Real Agent RPCs,
// scheduler admission, S3 reads/writes and durable ledgers exercise the exact
// queue-sharing gap: no new scan batch is needed to put the returner to work.
func TestReturningNodeSharesCompletedScanQueueWithLostReleaseReply(t *testing.T) {
	f := newAgentHTTPFixture(t)
	storage := &takeoverStorage{source: map[string][]byte{}, target: map[string][]byte{}, reads: map[string]int{}, writes: map[string]int{}}
	for i := 0; i < 6; i++ {
		storage.source[fmt.Sprintf("objects/%03d", i)] = bytes.Repeat([]byte{byte(i + 1)}, 2048)
	}
	gate, started := make(chan struct{}), make(chan struct{})
	var release, begin sync.Once
	t.Cleanup(func() { release.Do(func() { close(gate) }) })
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/source/objects/") {
			begin.Do(func() { close(started) })
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		storage.ServeHTTP(w, r)
	}))
	t.Cleanup(s3.Close)
	a, fault := handoffTestAgent(t, f, "queue-donor")
	b := takeoverTestAgent(t, f, "queue-returner")
	for _, agent := range []*takeoverAgent{a, b} {
		if err := agent.service.Engine.SetConcurrency(1, 1); err != nil {
			t.Fatal(err)
		}
	}
	code, body := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/s3-config", f.admin, map[string]any{"alias": "qa", "endpoint": s3.URL, "accessKey": "fixture-ak", "secretKey": "fixture-sk", "region": "test", "pathStyleEnable": true, "validate": false})
	if code != 200 {
		t.Fatal(code, body)
	}
	p := takeoverParent(t, f, a.node, b.node)
	runtime := f.master.tasks
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	snapshot, err := storageconfig.NewMaster(f.db).Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	available, err := runtime.prepareMembers(ctx, p, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	p, err = runtime.reconcileFailover(ctx, p, available)
	if err != nil {
		t.Fatal(err)
	}
	// The returning participant had no work; retire it before its rejoin.
	if err = b.service.Leases.Retire(ctx, p.ID, 0); err != nil {
		t.Fatal(err)
	}
	p, err = runtime.repo.BeginFailover(p.ID, p.Generation, b.node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = runtime.repo.PrepareFailover(p.ID, p.Generation, b.node.ID, map[string]bool{a.node.ID: true}); err != nil {
		t.Fatal(err)
	}
	var page []task.Assignment
	for i := 0; i < 6; i++ {
		page = append(page, task.Assignment{Number: i + 1, Objects: []transferstore.Object{{Key: fmt.Sprintf("objects/%03d", i), Size: 2048}}})
	}
	children, err := runtime.repo.AssignPageAvailable(p.ID, p.Generation, page, map[string]bool{a.node.ID: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = runtime.repo.ScanProgress(p.ID, p.Generation, 6, 6*2048, 6, true, ""); err != nil {
		t.Fatal(err)
	}
	if err = runtime.deliverChildren(ctx, p, children); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("donor did not start real source read")
	}
	// All other assignments are in the durable queue behind one running child.
	p, err = runtime.repo.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	p, err = runtime.rejoinAvailableNodes(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if p.NodeRetired(b.node.ID) || !p.ScanComplete {
		t.Fatal(p.Failover, p.ScanComplete)
	}
	if err = runtime.leases.renew(ctx, p, b.node); err != nil {
		t.Fatal(err)
	}
	available = map[string]bool{a.node.ID: true, b.node.ID: true}
	fault.loseReply.Store(true)
	if err = runtime.rebalanceQueued(ctx, p, available); err == nil {
		t.Fatal("lost release response was not exercised")
	}
	p, err = runtime.repo.Get(p.ID)
	if err != nil || p.Failover.Rebalance.Pending == nil {
		t.Fatal("missing durable handoff intent", err)
	}
	pending := *p.Failover.Rebalance.Pending
	id := fmt.Sprintf("%d-%d", p.ID, pending.Number)
	old, err := a.service.Jobs.Get(ctx, id)
	if err != nil || old.ReleasedAssignment == nil {
		t.Fatal("donor still owns lost-ACK work", old, err)
	}
	if _, err = b.service.Jobs.Get(ctx, id); err == nil {
		t.Fatal("target started before revocation ACK")
	}
	// Drop all repository object state, then replay the persisted operation.
	runtime.repo = task.New(f.db)
	if err = runtime.rebalanceQueued(ctx, p, available); err != nil {
		t.Fatal("handoff did not replay", err)
	}
	p, err = runtime.repo.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Scheduling.Nodes[a.node.ID].Children != 3 || p.Scheduling.Nodes[b.node.ID].Children != 3 {
		t.Fatal("queue load not balanced", p.Scheduling)
	}
	rows, err := runtime.repo.Children(p.ID, 0, 6)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Node.ID != a.node.ID || rows[0].Job.Assignment != 0 {
		t.Fatal("running child moved", rows[0])
	}
	if _, err = a.service.Engine.Accept(ctx, old.Job); err != execution.ErrAssignment {
		t.Fatal("delayed donor accept revived revoked work", err)
	}
	release.Do(func() { close(gate) })
	for {
		if err = runtime.observeAvailable(ctx, p, 64, available); err != nil {
			t.Fatal(err)
		}
		finished, err := runtime.repo.FinishIfReady(p.ID, p.Generation)
		if err != nil {
			t.Fatal(err)
		}
		if finished {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("transfers did not finish")
		case <-time.After(10 * time.Millisecond):
		}
	}
	storage.mu.Lock()
	defer storage.mu.Unlock()
	for key, want := range storage.source {
		if !bytes.Equal(storage.target[key], want) || storage.writes[key] != 1 {
			t.Fatal("duplicate/lost transfer", key, storage.writes[key])
		}
	}
	p, err = runtime.repo.Get(p.ID)
	if err != nil || p.Generation != 1 || p.Counters.Succeeded != 6 || p.Counters.SucceededBytes != 6*2048 || !p.ScanComplete {
		t.Fatal("task accounting changed", p, err)
	}
}
