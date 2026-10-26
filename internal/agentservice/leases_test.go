package agentservice

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

type inertLeaseTimer struct{}

func (inertLeaseTimer) Stop() bool { return true }
func leaseFixture(t *testing.T) (*state.Store, *Leases, *atomic.Int64) {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ClaimRole("agent"); err != nil {
		t.Fatal(err)
	}
	leases, err := NewLeases(db)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := &atomic.Int64{}
	origin := time.Now()
	leases.now = func() time.Time { return origin.Add(time.Duration(elapsed.Load())) }
	leases.afterFunc = func(time.Duration, func()) leaseTimer { return inertLeaseTimer{} }
	t.Cleanup(func() { leases.Close(); db.Close() })
	return db, leases, elapsed
}
func leaseRequest(parent int64, id string, sequence uint64) LeasePrepareRequest {
	return LeasePrepareRequest{ParentID: parent, Generation: 1, LeaseID: id, Sequence: sequence, TTLMillis: 1000}
}
func grantPrepared(t *testing.T, leases *Leases, req LeasePrepareRequest) LeaseGrantRequest {
	t.Helper()
	p, err := leases.Prepare(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	grant := LeaseGrantRequest{ParentID: req.ParentID, Generation: req.Generation, LeaseID: req.LeaseID, Sequence: req.Sequence, Challenge: p.Challenge, BootID: p.BootID}
	if err := leases.Grant(context.Background(), grant); err != nil {
		t.Fatal(err)
	}
	return grant
}
func TestLeaseFixedDeadlineReplayExpiryAndExplicitRearm(t *testing.T) {
	_, leases, elapsed := leaseFixture(t)
	req := leaseRequest(1, "first", 1)
	legacy, err := leases.ParentContext(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := leases.Prepare(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Err() == nil {
		t.Fatal("first prepare did not cancel legacy I/O")
	}
	elapsed.Store(int64(800 * time.Millisecond))
	grant := LeaseGrantRequest{ParentID: 1, Generation: 1, LeaseID: "first", Sequence: 1, Challenge: prepared.Challenge, BootID: prepared.BootID}
	if err := leases.Grant(context.Background(), grant); err != nil {
		t.Fatal(err)
	}
	running, err := leases.ParentContext(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	elapsed.Store(int64(950 * time.Millisecond))
	if err := leases.Grant(context.Background(), grant); err != nil {
		t.Fatal(err)
	}
	elapsed.Store(int64(1001 * time.Millisecond))
	if err := leases.Admission(1, 1); !errors.Is(err, ErrLeaseExpired) || running.Err() == nil {
		t.Fatalf("replayed grant extended fixed deadline: %v", err)
	}
	if err := leases.Grant(context.Background(), grant); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("late grant revived lease: %v", err)
	}
	req.Sequence++
	if _, err := leases.Prepare(context.Background(), req); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("same lease rearmed: %v", err)
	}
	grantPrepared(t, leases, leaseRequest(1, "replacement", 1))
	if err := leases.Admission(1, 1); err != nil {
		t.Fatal(err)
	}
}
func TestLeaseDelayedRenewCannotReviveExpiredOwner(t *testing.T) {
	_, leases, elapsed := leaseFixture(t)
	grantPrepared(t, leases, leaseRequest(1, "first", 1))
	elapsed.Store(int64(900 * time.Millisecond))
	req := leaseRequest(1, "first", 2)
	prepared, err := leases.Prepare(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	elapsed.Store(int64(1100 * time.Millisecond)) // candidate has time left, original owner does not.
	err = leases.Grant(context.Background(), LeaseGrantRequest{ParentID: 1, Generation: 1, LeaseID: "first", Sequence: 2, Challenge: prepared.Challenge, BootID: prepared.BootID})
	if !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("late renewal revived expired ownership: %v", err)
	}
}
func TestLeaseRestartAndRetirementAreDurable(t *testing.T) {
	db, leases, _ := leaseFixture(t)
	old := grantPrepared(t, leases, leaseRequest(1, "old", 1))
	leases.Close()
	reopened, err := NewLeases(db)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.Admission(1, 1); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("restart resumed old queue: %v", err)
	}
	if err := reopened.Grant(context.Background(), old); err == nil {
		t.Fatal("old boot grant accepted")
	}
	grantPrepared(t, reopened, leaseRequest(1, "new", 1))
	if err := reopened.Retire(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	again, err := NewLeases(db)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if _, err := again.Prepare(context.Background(), leaseRequest(1, "third", 1)); !errors.Is(err, ErrLeaseRetired) {
		t.Fatalf("retired parent revived after restart: %v", err)
	}
	if _, err := again.ParentContext(2, 1); err != nil {
		t.Fatalf("unrelated legacy parent affected: %v", err)
	}
}
func TestLeaseExpiryCancelsWhileDatabaseWriterBlocked(t *testing.T) {
	db, leases, _ := leaseFixture(t)
	leases.now = time.Now
	leases.afterFunc = func(d time.Duration, f func()) leaseTimer { return time.AfterFunc(d, f) }
	req := leaseRequest(1, "expiry", 1)
	req.TTLMillis = 80
	grantPrepared(t, leases, req)
	running, err := leases.ParentContext(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() { done <- db.Update(func(*state.Tx) error { close(entered); <-release; return nil }) }()
	<-entered
	select {
	case <-running.Done():
	case <-time.After(time.Second):
		close(release)
		<-done
		t.Fatal("lease expiry waited for DB writer")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
func TestLeaseSlowParentDoesNotBlockOtherRenewal(t *testing.T) {
	_, leases, _ := leaseFixture(t)
	grantPrepared(t, leases, leaseRequest(2, "healthy", 1))
	blocked, release := make(chan struct{}), make(chan struct{})
	leases.quiesce = func(ctx context.Context, parent int64) error {
		if parent == 1 {
			close(blocked)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	done := make(chan error, 1)
	go func() { _, err := leases.Prepare(context.Background(), leaseRequest(1, "slow", 1)); done <- err }()
	<-blocked
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	p, err := leases.Prepare(ctx, leaseRequest(2, "healthy", 2))
	close(release)
	if first := <-done; first != nil {
		t.Fatal(first)
	}
	if err != nil {
		t.Fatalf("other parent blocked renewal: %v", err)
	}
	if err := leases.Grant(ctx, LeaseGrantRequest{ParentID: 2, Generation: 1, LeaseID: "healthy", Sequence: 2, Challenge: p.Challenge, BootID: p.BootID}); err != nil {
		t.Fatal(err)
	}
}

func TestLeaseDeadlineCancelsEngineIOBeforeLedgerWriterReturns(t *testing.T) {
	db, leases, elapsed := leaseFixture(t)
	req := leaseRequest(7, "io", 1)
	grantPrepared(t, leases, req)
	repo, err := execution.NewRepository(db, execution.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	opened, canceled := make(chan struct{}), make(chan struct{})
	engine, err := execution.NewEngine(repo, execution.StoreFactoryFunc(func(ctx context.Context, _ execution.Job) (transferstore.Store, transferstore.Store, error) {
		close(opened)
		<-ctx.Done()
		close(canceled)
		return nil, nil, ctx.Err()
	}), execution.Options{ParentContext: leases.ParentContext})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close(context.Background())
	job := execution.Job{ChildID: "7-1", ParentID: 7, Number: 1, Generation: 1, Source: execution.Endpoint{Kind: "S3", Ref: "src", Bucket: "source"}, Destination: execution.Endpoint{Kind: "S3", Ref: "dst", Bucket: "target"}, ConflictPolicy: "OVERWRITE", DiscoveryComplete: true, Items: []execution.Item{{ID: "1", Source: transferstore.Object{Key: "object", Size: 1}, DestinationKey: "object"}}}
	if _, err := engine.AcceptSeed(context.Background(), job, nil); err != nil {
		t.Fatal(err)
	}
	if err := engine.TryStart(context.Background(), job.ChildID, 1); err != nil {
		t.Fatal(err)
	}
	<-opened
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() { done <- db.Update(func(*state.Tx) error { close(entered); <-release; return nil }) }()
	<-entered
	elapsed.Store(int64(1001 * time.Millisecond))
	if err := leases.Admission(7, 1); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("lease did not expire: %v", err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		close(release)
		<-done
		t.Fatal("active engine I/O survived lease while ledger writer was blocked")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	record, err := engine.Wait(ctx, job.ChildID)
	if err != nil || record.Status != execution.Recoverable {
		t.Fatalf("canceled I/O did not preserve recoverable ledger: %+v %v", record, err)
	}
	if err := engine.TryRecover(ctx, job.ChildID, 1); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("expired queued work resumed: %v", err)
	}
}
