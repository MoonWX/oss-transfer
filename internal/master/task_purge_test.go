package master

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/cluster"
	"github.com/MoonWX/oss-transfer/internal/task"
)

func purgeFixtureTask(t *testing.T, f *agentHTTPFixture, session agenttransport.Session, requestKey string) task.Parent {
	t.Helper()
	spec, _, _, err := normalizeTaskRequest(ossTaskRequest(), "add-buckets")
	if err != nil {
		t.Fatal(err)
	}
	spec.Nodes = []task.Node{{ID: session.NodeID, EnrollmentID: session.EnrollmentID, Address: "node.example"}}
	p, _, err := f.master.tasks.repo.Create(spec, requestKey)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func stopAndRecyclePurgeFixture(t *testing.T, f *agentHTTPFixture, p task.Parent) {
	t.Helper()
	// This fixture has never dispatched a child. Real running tasks must obtain
	// each Agent's stop acknowledgement before Repository.Stopped is called.
	if _, err := f.master.tasks.repo.Stop(p.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.master.tasks.repo.Stopped(p.ID, p.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := f.master.tasks.repo.Recycle(p.ID, true); err != nil {
		t.Fatal(err)
	}
}

func TestPurgeRetiredEnrollmentDoesNotContactReplacementOrReviveCredential(t *testing.T) {
	f := newAgentHTTPFixture(t)
	credential := agentCredential(t)
	status, original := f.join(t, f.invite(t, "node.example"), "same-instance", credential)
	if status != http.StatusOK {
		t.Fatal(status)
	}
	p := purgeFixtureTask(t, f, original, "old-task")
	status, body := request(t, f.http.Client(), "DELETE", f.http.URL+"/api/master/nodes/node.example", f.admin, nil)
	if status != http.StatusConflict {
		t.Fatal("active task did not prevent Agent removal", status, body)
	}
	stopAndRecyclePurgeFixture(t, f, p)
	status, body = request(t, f.http.Client(), "DELETE", f.http.URL+"/api/master/nodes/node.example", f.admin, nil)
	if status != http.StatusOK {
		t.Fatal("stopped task prevented Agent removal", status, body)
	}
	status, replacement := f.join(t, f.invite(t, "node.example"), "same-instance", agentCredential(t))
	if status != http.StatusOK || replacement == original {
		t.Fatal("replacement enrollment failed", status)
	}
	newTask := purgeFixtureTask(t, f, replacement, "replacement-task")
	if _, err := f.master.agents.registry.Authenticate(credential); !errors.Is(err, cluster.ErrUnauthorized) {
		t.Fatal("revoked credential authenticated after replacement", err)
	}
	if status := f.agentStatus(t, "GET", agenttransport.PollPath, credential, nil); status != http.StatusUnauthorized {
		t.Fatal("old Agent reconnected", status)
	}
	status, body = request(t, f.http.Client(), "POST", f.http.URL+"/api/master/permanent-delete", f.admin, map[string]any{"taskIds": []int64{p.ID}})
	if status != http.StatusOK || body["accepted"] != float64(1) || body["failed"] != float64(0) {
		t.Fatal("retired Agent locked its task in recycle bin", status, body)
	}
	item := body["details"].([]any)[0].(map[string]any)
	if !reflect.DeepEqual(item["retainedAgentHistory"], []any{"node.example"}) || item["message"] == "" {
		t.Fatal("purge concealed retained retired-Agent history", item)
	}
	purged, err := f.master.tasks.repo.Get(p.ID)
	if err != nil || purged.Purged || !purged.Deleted || purged.Cleanup == nil {
		t.Fatal("durable cleanup admission missing", purged, err)
	}
	for !purged.Purged {
		if err := f.master.tasks.cleanup.step(context.Background(), p.ID); err != nil {
			t.Fatal(err)
		}
		purged, err = f.master.tasks.repo.Get(p.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	unchanged, err := f.master.tasks.repo.Get(newTask.ID)
	if err != nil || !reflect.DeepEqual(newTask, unchanged) {
		t.Fatal("old purge modified replacement's active task", unchanged, err)
	}
	if _, err := f.master.controlTask(context.Background(), p.ID, "resume"); err == nil {
		t.Fatal("purged old task resumed")
	}
	if _, err := f.master.controlTask(context.Background(), p.ID, "permanent-delete"); err != nil {
		t.Fatal("lost purge response could not be retried", err)
	}
}

func TestPurgeOfflineOrUnknownEnrollmentRequiresCleanupAcknowledgement(t *testing.T) {
	f := newAgentHTTPFixture(t)
	status, registered := f.join(t, f.invite(t, "node.example"), "offline-instance", agentCredential(t))
	if status != http.StatusOK {
		t.Fatal(status)
	}
	for name, session := range map[string]agenttransport.Session{
		"offline": registered,
		"unknown": {NodeID: "unknown-node", EnrollmentID: "unknown-enrollment"},
	} {
		t.Run(name, func(t *testing.T) {
			p := purgeFixtureTask(t, f, session, name)
			stopAndRecyclePurgeFixture(t, f, p)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := f.master.controlTask(ctx, p.ID, "permanent-delete"); err != nil {
				t.Fatal("offline node prevented durable admission", err)
			}
			f.master.tasks.cleanup.nodeTimeout = 20 * time.Millisecond
			if err := f.master.tasks.cleanup.step(ctx, p.ID); err != nil {
				t.Fatal(err)
			}
			retained, err := f.master.tasks.repo.Get(p.ID)
			if err != nil || retained.Purged || !retained.Deleted || retained.Cleanup == nil || retained.Cleanup.Status != "WAITING" {
				t.Fatal("unconfirmed cleanup lost recycle-bin task", retained, err)
			}
		})
	}
}
