package master

import (
	"context"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/softwareupdate"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
	"github.com/MoonWX/oss-transfer/internal/task"
)

func TestSoftwareUpdateAdmissionAndMaintenanceExpireTogether(t *testing.T) {
	f := newAgentHTTPFixture(t)
	f.close()
	now := time.Now().UTC()
	openMaintenanceFixture(t, f, func() time.Time { return now })
	for _, tc := range []struct {
		name    string
		started time.Time
		blocked bool
	}{
		{"before deadline", now.Add(-agentMaintenanceTTL + time.Nanosecond), true},
		{"at deadline", now.Add(-agentMaintenanceTTL), false},
		{"after deadline", now.Add(-agentMaintenanceTTL - time.Nanosecond), false},
		{"missing start time", time.Time{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := agentUpdateQueue{Phase: "RUNNING", Items: []agentUpdateItem{{NodeID: "updating", Phase: "UPDATING", StartedAt: tc.started}}}
			if err := f.master.saveSoftwareQueue(q); err != nil {
				t.Fatal(err)
			}
			if blocked := f.master.softwareUpdateBlocksNode("updating"); blocked != tc.blocked {
				t.Fatalf("admission blocked=%v, want %v", blocked, tc.blocked)
			}
			if protected, err := f.master.agentUnderMaintenance("updating"); err != nil || protected != tc.blocked {
				t.Fatalf("maintenance=%v, error=%v, want %v", protected, err, tc.blocked)
			}
		})
	}
}

func TestExpiredSoftwareUpdateDoesNotExcludeOnlineAgent(t *testing.T) {
	f := newAgentHTTPFixture(t)
	f.master.options.Updater = &fakeSoftwareUpdater{info: softwareupdate.Info{Role: "master", Version: "4.1.15", Supported: true}}
	u := &fakeSoftwareUpdater{info: softwareupdate.Info{Role: "agent", Version: "4.1.15", Supported: true}, status: softwareupdate.Status{Phase: "IDLE"}}
	node := nodeTestAgentWithUpdater(t, f, "expired-update-online", u)
	member, err := f.master.nodeByAddress(node.address)
	if err != nil {
		t.Fatal(err)
	}
	p := takeoverParent(t, f, task.Node{ID: member.ID, EnrollmentID: member.EnrollmentID, Address: member.Address})
	q := agentUpdateQueue{Version: "4.1.15", Phase: "RUNNING", Items: []agentUpdateItem{{
		NodeID: member.ID, EnrollmentID: member.EnrollmentID, Address: member.Address,
		Phase: "UPDATING", StartedAt: time.Now().Add(-31 * time.Minute), UpdateID: "missing-result",
	}}}
	if err := f.master.saveSoftwareQueue(q); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Exercise the actual authenticated Agent channel before the queue loop
	// marks its stale entry failed. No failover should be needed to use this node.
	available, err := f.master.tasks.prepareMembers(ctx, p, storageconfig.Snapshot{Revision: 1})
	if err != nil || !available[member.ID] {
		t.Fatal("expired update still excluded a reachable Agent", available, err)
	}
	f.master.stepSoftwareQueue(ctx)
	q, err = f.master.softwareQueue()
	if err != nil || q.Phase != "FAILED" || q.Items[0].Reason != "TIMEOUT" {
		t.Fatal("online update queue did not expire at 30 minutes", q, err)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.started) != 0 {
		t.Fatal("expired update was dispatched again", u.started)
	}
}
