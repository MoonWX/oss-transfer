package master

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestRetryHTTPPersistsAcceptanceWithoutWaitingForOfflineAgent(t *testing.T) {
	f := newAgentHTTPFixture(t)
	status, session := f.join(t, f.invite(t, "offline.example"), "offline-retry", agentCredential(t))
	if status != http.StatusOK {
		t.Fatal(status)
	}
	p := purgeFixtureTask(t, f, session, "retry-offline")
	if _, err := f.master.tasks.repo.Stop(p.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.master.tasks.repo.Stopped(p.ID, p.Generation); err != nil {
		t.Fatal(err)
	}
	// The Agent never polls. The HTTP call must still return its durable
	// acceptance; waiting for any command acknowledgement would exceed this.
	client := *f.http.Client()
	client.Timeout = time.Second
	for _, route := range []string{"retry", "resume", "local-transfers/retry-failures"} {
		status, body := request(t, &client, "POST", f.http.URL+"/api/master/"+route, f.admin, map[string]any{"taskIds": []int64{p.ID}})
		if data, ok := body["data"].(map[string]any); ok {
			body = data
		}
		if status != http.StatusOK || body["accepted"] != float64(1) || body["failed"] != float64(0) {
			t.Fatal(route, status, body)
		}
		detail := body["details"].([]any)[0].(map[string]any)
		if detail["status"] != "RESUMING" {
			t.Fatal(detail)
		}
		current, err := f.master.tasks.repo.Get(p.ID)
		if err != nil || current.Generation != 2 || current.Status != "RESUMING" {
			t.Fatal(current, err)
		}
	}
	// Preparation is durable independently of the request's cancelled context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.master.tasks.step(ctx, p.ID); err == nil {
		t.Fatal("cancelled preparation continued")
	}
	current, _ := f.master.tasks.repo.Get(p.ID)
	if current.ResumeAfter == nil {
		t.Fatal("cancelled step consumed recovery cursor")
	}
	f.close()
	f.open(t)
	restored, err := f.master.tasks.repo.Get(p.ID)
	if err != nil || restored.Status != "RESUMING" || restored.Generation != 2 || restored.ResumeAfter == nil {
		t.Fatal(restored, err)
	}
}
