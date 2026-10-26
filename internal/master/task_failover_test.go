package master

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

type takeoverExecutor struct{ *agentservice.Service }

func (a takeoverExecutor) Execute(ctx context.Context, c agenttransport.Command) (agenttransport.Response, error) {
	if c.Operation == "capabilities" {
		raw, _ := json.Marshal(a.LeaseCapabilities())
		return agenttransport.Response{Status: 200, Payload: raw}, nil
	}
	return a.Service.Execute(ctx, c)
}

type takeoverAgent struct {
	node    task.Node
	service *agentservice.Service
	cancel  context.CancelFunc
	done    chan error
}

func takeoverTestAgent(t *testing.T, f *agentHTTPFixture, name string) *takeoverAgent {
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
	runner := agenttransport.NewRunner(agentClient(t, f, credential), takeoverExecutor{service}, nil, agenttransport.RunnerOptions{ReconnectDelay: time.Millisecond})
	go func() { a.done <- runner.Run(ctx) }()
	t.Cleanup(func() { a.cancel(); <-a.done; service.Close(context.Background()); db.Close() })
	return a
}
func takeoverParent(t *testing.T, f *agentHTTPFixture, nodes ...task.Node) task.Parent {
	t.Helper()
	p, _, err := f.master.tasks.repo.Create(task.Spec{Direction: "OSS_TO_OSS", Source: execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "source"}, Destination: execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "target"}, Nodes: nodes, ConflictPolicy: "SKIP", DistributionMode: "BALANCED", BandwidthMbps: 8}, "takeover")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Real Agent lease protocol over authenticated outbound HTTP. The test moves
// the Master's monotonic fence independently of Agent execution: the latter
// must already have cancelled itself when the coordinator permits migration.
func TestFailoverLeaseFenceMigrationAndReturningOwner(t *testing.T) {
	f := newAgentHTTPFixture(t)
	a, b := takeoverTestAgent(t, f, "failover-a"), takeoverTestAgent(t, f, "failover-b")
	p := takeoverParent(t, f, a.node, b.node)
	child, err := f.master.tasks.repo.Assign(p.ID, p.Generation, 1, []transferstore.Object{{Key: "keep", Size: 3}})
	if err != nil {
		t.Fatal(err)
	}
	owner, other := a, b
	if child.Node.ID == b.node.ID {
		owner, other = b, a
	}
	runtime := f.master.tasks
	runtime.leases.ttl = time.Second
	runtime.leases.margin = 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	available := map[string]bool{a.node.ID: true, b.node.ID: true}
	p, err = runtime.reconcileFailover(ctx, p, available)
	if err != nil {
		t.Fatal(err)
	}
	if p.Failover == nil || !p.Failover.Enabled {
		t.Fatal("new Agents did not enable leases")
	}
	if _, err := owner.service.Leases.ParentContext(p.ID, p.Generation); err != nil {
		t.Fatal(err)
	}
	// Unavailable control channel does not itself revoke I/O. Before expiry,
	// ownership and budget must remain unchanged.
	available = map[string]bool{other.node.ID: true}
	p, err = runtime.reconcileFailover(ctx, p, available)
	if err != nil {
		t.Fatal(err)
	}
	if p.NodeRetired(owner.node.ID) {
		t.Fatal("task moved before its lease expired")
	}
	time.Sleep(1200 * time.Millisecond)
	if _, err := owner.service.Leases.ParentContext(p.ID, p.Generation); err == nil {
		t.Fatal("old Agent still authorized after expiry")
	}
	p, err = runtime.reconcileFailover(ctx, p, available)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := runtime.repo.Children(p.ID, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !p.NodeRetired(owner.node.ID) || rows[0].Node.ID != other.node.ID || rows[0].Job.ChildID != child.Job.ChildID || rows[0].Job.Assignment != 1 {
		t.Fatal("assignment was not durably transferred", p, rows)
	}
	if parentPolicy(p, 0).BandwidthBytesPerSecond != 1000000 {
		t.Fatal("retired member retained bandwidth")
	}
	// A returning peer cannot acquire this parent's ownership again.
	if err := runtime.leases.renew(ctx, p, owner.node); err == nil {
		t.Fatal("retired owner received another lease")
	}
	if err := runtime.repo.Observe(p.ID, p.Generation, child.Job.Number, execution.Record{Job: child.Job, Status: execution.Completed, Attempt: 1, UpdatedAt: time.Now(), Counters: execution.Counters{Total: 1, TotalBytes: 3, Succeeded: 1, SucceededBytes: 3}}); err == nil {
		t.Fatal("late old-owner report was accepted")
	}
}

func TestFailoverMasterRestartRestoresFullConservativeWait(t *testing.T) {
	f := newAgentHTTPFixture(t)
	a, b := takeoverTestAgent(t, f, "restart-a"), takeoverTestAgent(t, f, "restart-b")
	p := takeoverParent(t, f, a.node, b.node)
	leases := f.master.tasks.leases
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := leases.renew(ctx, p, a.node); err != nil {
		t.Fatal(err)
	}
	// A stale/past wall-clock deadline must not authorize immediate takeover.
	key := taskLeaseKey(p.ID, a.node.ID)
	if err := f.db.Update(func(tx *state.Tx) error {
		var r taskLeaseRecord
		if _, err := tx.Get("master-task-leases", key, &r); err != nil {
			return err
		}
		r.Deadline = time.Unix(1, 0)
		return tx.Put("master-task-leases", key, r)
	}); err != nil {
		t.Fatal(err)
	}
	fresh := newTaskLeases(f.master.tasks)
	if expired, err := fresh.expired(p, a.node); err != nil || expired {
		t.Fatal("restart trusted a wall clock deadline", expired, err)
	}
	e, err := fresh.entry(p, a.node)
	if err != nil {
		t.Fatal(err)
	}
	if time.Until(e.safeAfter) < taskLeaseTTL {
		t.Fatal("restart did not preserve full lease fence")
	}
}

func TestFailoverSharedProofCannotBeRecreatedOnWrongMount(t *testing.T) {
	f := newAgentHTTPFixture(t)
	a, b := takeoverTestAgent(t, f, "proof-a"), takeoverTestAgent(t, f, "proof-b")
	root := t.TempDir()
	a.node.LocalRoot = root
	b.node.LocalRoot = root
	p := takeoverParent(t, f, a.node, b.node)
	p.Spec.Source = execution.Endpoint{Kind: "LOCAL", Root: root}
	p.Spec.Direction = "LOCAL_TO_OSS"
	p.Spec.SharedLocalSource = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.master.tasks.verifyTaskStorage(ctx, p, p.Spec.Nodes, true); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 {
		t.Fatal("persistent marker missing", entries, err)
	}
	wrong := t.TempDir()
	b.node.LocalRoot = wrong
	if err := f.master.tasks.verifyTaskStorage(ctx, p, []task.Node{b.node}, false); err == nil {
		t.Fatal("replacement directory falsely proved same shared volume")
	}
	entries, err = os.ReadDir(wrong)
	if err != nil || len(entries) != 0 {
		t.Fatal("failed takeover created marker on wrong volume", entries, err)
	}
}
