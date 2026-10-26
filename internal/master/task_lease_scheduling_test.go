package master

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/task"
)

func leaseSchedulingFixture(t *testing.T, n int) (*taskLeases, task.Parent) {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "master.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ClaimRole("master"); err != nil {
		t.Fatal(err)
	}
	runtime := newTaskRuntime(&Server{store: db})
	nodes := make([]task.Node, n)
	for i := range nodes {
		nodes[i] = task.Node{ID: fmt.Sprintf("node-%d", i), EnrollmentID: fmt.Sprintf("enrollment-%d", i)}
	}
	parent, _, err := runtime.repo.Create(task.Spec{Direction: "OSS_TO_OSS", Source: execution.Endpoint{Kind: "S3", Ref: "source", Bucket: "source"}, Destination: execution.Endpoint{Kind: "S3", Ref: "target", Bucket: "target"}, Nodes: nodes, ConflictPolicy: "SKIP", DistributionMode: "BALANCED"}, "lease-scheduling")
	if err != nil {
		t.Fatal(err)
	}
	parent, err = runtime.repo.EnableFailover(parent.ID, parent.Generation, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runtime.cancel(); db.Close() })
	return runtime.leases, parent
}
func TestLeaseExpiredPeerRearmsWithoutShorteningOutstandingFence(t *testing.T) {
	for _, stage := range []string{"lease_prepare", "lease_grant"} {
		t.Run(stage, func(t *testing.T) {
			leases, parent := leaseSchedulingFixture(t, 2)
			now := time.Now()
			leases.now = func() time.Time { return now }
			node := parent.Spec.Nodes[0]
			entry, err := leases.entry(parent, node)
			if err != nil {
				t.Fatal(err)
			}
			oldFence := now.Add(5 * time.Minute)
			entry.record.LeaseID = "before-agent-restart"
			entry.record.Sequence = 8
			entry.record.Armed = true
			entry.safeAfter = oldFence
			prepared, granted := 0, 0
			rejected := false
			leases.call = func(ctx context.Context, _ agenttransport.Session, operation string, _ bool, value any, into any) error {
				if operation == stage && !rejected {
					rejected = true
					return &AgentError{Status: 409, Message: agentservice.ErrLeaseExpired.Error()}
				}
				switch operation {
				case "lease_prepare":
					prepared++
					request := value.(agentservice.LeasePrepareRequest)
					if rejected && request.LeaseID == "before-agent-restart" {
						t.Fatal("expired lease retried without a new ID")
					}
					*into.(*agentservice.LeasePrepared) = agentservice.LeasePrepared{Challenge: fmt.Sprintf("proof-%d", prepared), BootID: "fresh-boot", TTLMillis: request.TTLMillis}
				case "lease_grant":
					granted++
					if err := leases.runtime.server.store.View(func(tx *state.Tx) error {
						var saved taskLeaseRecord
						_, err := tx.Get("master-task-leases", taskLeaseKey(parent.ID, node.ID), &saved)
						if !saved.Armed || saved.Deadline.Before(oldFence) {
							t.Errorf("grant preceded its durable conservative fence: %+v", saved)
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
				default:
					t.Fatalf("unexpected operation %s", operation)
				}
				return nil
			}
			if err := leases.renew(context.Background(), parent, node); err != nil {
				t.Fatal(err)
			}
			if !rejected || granted != 1 || entry.record.LeaseID == "before-agent-restart" || entry.safeAfter.Before(oldFence) {
				t.Fatalf("rearm lost ownership fence: %+v rejected=%v grants=%d", entry, rejected, granted)
			}
		})
	}
}
func TestLeaseWorkersRenewHealthyPeerWhileManyPeersRemainBlocked(t *testing.T) {
	const slowCount = 33
	leases, parent := leaseSchedulingFixture(t, slowCount+1)
	leases.ttl = 90 * time.Millisecond
	leases.margin = 10 * time.Millisecond
	leases.retryInterval = 5 * time.Millisecond
	for _, node := range parent.Spec.Nodes {
		if _, err := leases.entry(parent, node); err != nil {
			t.Fatal(err)
		}
	}
	healthy := parent.Spec.Nodes[slowCount]
	blocked := make(chan string, slowCount)
	healthyGrants := make(chan struct{}, 16)
	var mu sync.Mutex
	active := map[string]int{}
	overlapping := false
	leases.call = func(ctx context.Context, node agenttransport.Session, operation string, _ bool, value any, into any) error {
		mu.Lock()
		active[node.NodeID]++
		if active[node.NodeID] > 1 {
			overlapping = true
		}
		mu.Unlock()
		defer func() { mu.Lock(); active[node.NodeID]--; mu.Unlock() }()
		if node.NodeID != healthy.ID {
			select {
			case blocked <- node.NodeID:
			default:
			}
			<-ctx.Done()
			return ctx.Err()
		}
		if operation == "lease_prepare" {
			req := value.(agentservice.LeasePrepareRequest)
			*into.(*agentservice.LeasePrepared) = agentservice.LeasePrepared{Challenge: fmt.Sprint(req.Sequence), BootID: "healthy", TTLMillis: req.TTLMillis}
		} else if operation == "lease_grant" {
			select {
			case healthyGrants <- struct{}{}:
			default:
			}
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { leases.loop(ctx); close(done) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("lease workers survived manager shutdown")
		}
	}()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for range slowCount {
		select {
		case <-blocked:
		case <-deadline.C:
			t.Fatal("slow peers occupied a global worker limit")
		}
	}
	for range 2 {
		select {
		case <-healthyGrants:
		case <-deadline.C:
			t.Fatal("healthy renewal waited for slow peers")
		}
	}
	mu.Lock()
	overlap := overlapping
	mu.Unlock()
	if overlap {
		t.Fatal("more than one request ran for the same lease")
	}
	// Ending a task parks the healthy worker without issuing another command.
	if _, err := leases.runtime.repo.Stop(parent.ID); err != nil {
		t.Fatal(err)
	}
	if err := leases.runtime.repo.Stopped(parent.ID, parent.Generation); err != nil {
		t.Fatal(err)
	}
	entry, err := leases.entry(parent, healthy)
	if err != nil {
		t.Fatal(err)
	}
	if _, park := leases.leaseTick(ctx, entry); !park {
		t.Fatal("finished task continued its renewal timer")
	}
}

func TestLeaseStaleRunningSnapshotCannotRenewStoppedTask(t *testing.T) {
	leases, parent := leaseSchedulingFixture(t, 2)
	calls := 0
	leases.call = func(context.Context, agenttransport.Session, string, bool, any, any) error { calls++; return nil }
	if _, err := leases.runtime.repo.Stop(parent.ID); err != nil {
		t.Fatal(err)
	}
	if err := leases.renew(context.Background(), parent, parent.Spec.Nodes[0]); !errors.Is(err, task.ErrState) {
		t.Fatalf("stale RUNNING snapshot renewed STOPPING task: %v", err)
	}
	if calls != 0 {
		t.Fatalf("stale snapshot reached Agent %d times", calls)
	}
	if err := leases.runtime.repo.Stopped(parent.ID, parent.Generation); err != nil {
		t.Fatal(err)
	}
	resumed, err := leases.runtime.repo.Resume(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Generation == parent.Generation {
		t.Fatal("resume did not advance generation")
	}
	if err := leases.renew(context.Background(), parent, parent.Spec.Nodes[0]); !errors.Is(err, task.ErrGeneration) {
		t.Fatalf("old generation renewed after resume: %v", err)
	}
	if calls != 0 {
		t.Fatalf("stale generation reached Agent %d times", calls)
	}
}
