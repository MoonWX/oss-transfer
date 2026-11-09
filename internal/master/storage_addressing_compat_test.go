package master

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/cluster"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
)

// addressingAgent applies storage snapshots like an Agent does, with its own
// database. A previous one (v4.1.x) advertises no storageAddressingProtocol,
// reads an unset pathStyleEnable as virtual-hosted and predates storage_check.
type addressingAgent struct {
	current bool
	store   *storageconfig.Agent
	mu      sync.Mutex
	applied []storageconfig.Snapshot
	checked []storageconfig.Config
}

func newAddressingAgent(t *testing.T, current bool) *addressingAgent {
	db, err := state.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &addressingAgent{current: current, store: storageconfig.NewAgent(db)}
}

func (a *addressingAgent) ReplaySafe(string) bool { return true }

func (a *addressingAgent) Execute(_ context.Context, c agenttransport.Command) (agenttransport.Response, error) {
	switch c.Operation {
	case "capabilities":
		caps := map[string]any{"leaseProtocol": 2}
		if a.current {
			caps["storageAddressingProtocol"] = 1
		}
		return fakeReply(200, caps)
	case "storage_apply":
		var snapshot storageconfig.Snapshot
		if err := fakeDecode(c.Payload, &snapshot); err != nil {
			return fakeReply(400, map[string]string{"code": "invalid_request", "message": err.Error()})
		}
		a.mu.Lock()
		a.applied = append(a.applied, snapshot)
		a.mu.Unlock()
		if _, err := a.store.ApplySnapshot(snapshot); err != nil {
			return fakeReply(409, map[string]string{"code": "operation_failed", "message": err.Error()})
		}
		return fakeReply(200, map[string]any{"revision": snapshot.Revision})
	case "storage_buckets":
		var config storageconfig.Config
		if err := fakeDecode(c.Payload, &config); err != nil {
			return fakeReply(400, map[string]string{"code": "invalid_request", "message": err.Error()})
		}
		a.mu.Lock()
		a.checked = append(a.checked, config)
		a.mu.Unlock()
		return fakeReply(200, map[string]any{"buckets": []string{"src"}})
	}
	return fakeUnknownOperation()
}

func (a *addressingAgent) held(t *testing.T) storageconfig.Snapshot {
	t.Helper()
	snapshot, err := a.store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

// pathStyleOf is the addressing an Agent of this kind uses for alias.
func (a *addressingAgent) pathStyleOf(t *testing.T, alias string) bool {
	t.Helper()
	for _, config := range a.held(t).Connections {
		if config.Alias == alias {
			if a.current {
				return config.EffectivePathStyle()
			}
			return config.PathStyleEnable != nil && *config.PathStyleEnable // the v4.1 rule
		}
	}
	t.Fatalf("connection %s not applied", alias)
	return false
}

// Automatic addressing reaches an Agent that predates it as the addressing it
// resolves to, so it uses what the console shows. A revision that a v4.1
// Master applied before the upgrade stays as that Agent holds it, and an
// Agent upgraded after receiving resolved revisions still accepts them.
func TestStorageSyncResolvesAutomaticAddressingForPreviousAgents(t *testing.T) {
	f := newAgentHTTPFixture(t)
	ctx := context.Background()
	previous := newAddressingAgent(t, false)
	node := comparisonTestNode(t, f, "addressing-previous", previous)
	member := cluster.Member{ID: node.ID, EnrollmentID: node.EnrollmentID}
	master := storageconfig.NewMaster(f.db)

	// Revision 1 was applied by a v4.1 Master, unresolved.
	first, err := master.Create(storageconfig.Config{Alias: "minio", Endpoint: "http://minio:9000", AccessKey: "fixture-ak", SecretKey: "fixture-sk"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := previous.store.ApplySnapshot(first); err != nil {
		t.Fatal(err)
	}
	if err := f.master.syncStorage(ctx, member, first); err != nil {
		t.Fatalf("a revision applied before the upgrade was refused: %v", err)
	}

	// A connection saved after the upgrade: the previous Agent gets path-style
	// where automatic resolves to it, and nothing where it resolves to
	// virtual-hosted (which that Agent already uses for an unset choice).
	second, err := master.Create(storageconfig.Config{Alias: "aws", Endpoint: "https://s3.us-east-1.amazonaws.com", AccessKey: "fixture-ak", SecretKey: "fixture-sk"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.master.syncStorage(ctx, member, second); err != nil {
		t.Fatal(err)
	}
	if previous.held(t).Revision != second.Revision || !previous.pathStyleOf(t, "minio") || previous.pathStyleOf(t, "aws") {
		t.Fatalf("previous Agent holds %+v", previous.held(t))
	}
	for _, config := range previous.held(t).Connections {
		if config.Alias == "aws" && config.PathStyleEnable != nil {
			t.Fatal("an unset choice that resolves to virtual-hosted was changed")
		}
	}

	// The connection check on that Agent lists buckets with the addressing
	// that the node uses, and reports it.
	outcome, err := f.master.checkStorage(ctx, second.Connections[1], "")
	if err != nil || outcome.complete || !outcome.pathStyle {
		t.Fatalf("check: %+v %v", outcome, err)
	}
	previous.mu.Lock()
	checked := append([]storageconfig.Config(nil), previous.checked...)
	previous.mu.Unlock()
	if len(checked) != 1 || checked[0].PathStyleEnable == nil || !*checked[0].PathStyleEnable {
		t.Fatalf("the previous Agent listed buckets with %+v", checked)
	}

	// A current Agent receives the unset choice. One that received the
	// resolved revision while it was still old accepts the same revision.
	current := newAddressingAgent(t, true)
	upgraded := newAddressingAgent(t, true)
	resolved, differs := storageAddressingSnapshot(second)
	if !differs {
		t.Fatal("fixture has nothing to resolve")
	}
	if _, err := upgraded.store.ApplySnapshot(resolved); err != nil {
		t.Fatal(err)
	}
	for name, agent := range map[string]*addressingAgent{"addressing-current": current, "addressing-upgraded": upgraded} {
		node := comparisonTestNode(t, f, name, agent)
		if err := f.master.syncStorage(ctx, cluster.Member{ID: node.ID, EnrollmentID: node.EnrollmentID}, second); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !agent.pathStyleOf(t, "minio") || agent.pathStyleOf(t, "aws") {
			t.Fatalf("%s holds %+v", name, agent.held(t))
		}
	}
	for _, config := range current.held(t).Connections {
		if config.PathStyleEnable != nil {
			t.Fatalf("a current Agent received a resolved choice: %+v", config)
		}
	}
	current.mu.Lock()
	raw, _ := json.Marshal(current.applied)
	count := len(current.applied)
	current.mu.Unlock()
	if count != 1 {
		t.Fatalf("current Agent applied %s", raw)
	}
}
