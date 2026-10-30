package main

import (
	"bytes"
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMaintenanceReadEndpointsAndNoImplicitUpgrade(t *testing.T) {
	for _, test := range []struct {
		group        string
		args         []string
		method, path string
		payload      map[string]interface{}
	}{
		{"update", []string{"status"}, "GET", "/api/master/software-updates", map[string]interface{}{"checking": true, "queue": map[string]interface{}{"phase": "RUNNING"}}},
		{"update", []string{"check"}, "POST", "/api/master/software-updates/check", map[string]interface{}{"version": "4.1.22"}},
		{"backup", []string{"list"}, "GET", "/api/master/software-updates/backups", map[string]interface{}{"master": map[string]interface{}{"supported": false, "reason": "BUSY"}}},
	} {
		t.Run(test.group+"/"+test.args[0], func(t *testing.T) {
			calls := 0
			client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != test.method || r.URL.Path != test.path {
					t.Fatalf("unexpected request %s %s", r.Method, r.URL)
				}
				if test.method == "POST" && len(adminTestBody(t, r)) != 0 {
					t.Fatal("check must not submit an upgrade")
				}
				adminTestJSON(t, w, map[string]interface{}{"success": true, "data": test.payload})
			})
			result, err := runMaintenanceCommand(client, test.group, test.args)
			if err != nil || calls != 1 || !reflect.DeepEqual(adminData(result), test.payload) {
				t.Fatalf("calls=%d result=%#v err=%v", calls, result, err)
			}
		})
	}
}

func TestMaintenanceMutationsRequireExplicitTargetAndConfirmation(t *testing.T) {
	cases := []struct {
		group string
		args  []string
	}{
		{"update", nil}, {"update", []string{"upgrade"}}, {"update", []string{"check", "--yes"}},
		{"update", []string{"master", "--version", "4.1.22"}}, {"update", []string{"master", "--yes"}},
		{"update", []string{"master", "--version", "latest", "--yes"}}, {"update", []string{"master", "--version", "4.1.22-rc.1", "--yes"}},
		{"update", []string{"master", "--version", "4.1.22", "--worker", "node-a", "--yes"}},
		{"update", []string{"master", "--version", "4.1.22", "--all", "--yes"}},
		{"update", []string{"agent", "--yes"}}, {"update", []string{"agent", "--all", "--yes"}},
		{"update", []string{"agent", "--worker", "node-a"}}, {"update", []string{"agent", "--worker", "node-a/path", "--yes"}},
		{"update", []string{"agent", "--worker", "node-a", "--worker", "node-b", "--yes"}},
		{"update", []string{"agents", "--yes"}}, {"update", []string{"agents", "--all", "--worker", "node-a", "--yes"}},
		{"update", []string{"agents", "--all", "--version", "4.1.22", "--version", "4.1.23", "--yes"}},
		{"backup", nil}, {"backup", []string{"list", "--yes"}}, {"backup", []string{"prune", "--yes"}},
		{"backup", []string{"prune", "master"}}, {"backup", []string{"prune", "master", "--worker", "node-a", "--yes"}},
		{"backup", []string{"prune", "agent", "--yes"}}, {"backup", []string{"prune", "agent", "--worker", "", "--yes"}},
		{"backup", []string{"prune", "all", "--yes"}},
	}
	for _, test := range cases {
		t.Run(test.group+"/"+strings.Join(test.args, " "), func(t *testing.T) {
			client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				t.Fatalf("invalid command made request: %s %s", r.Method, r.URL)
			})
			if _, err := runMaintenanceCommand(client, test.group, test.args); err == nil {
				t.Fatalf("accepted %v", test.args)
			}
		})
	}
}

func TestMaintenanceUpgradeSubmissionPreservesQueuedState(t *testing.T) {
	for _, test := range []struct {
		args       []string
		path       string
		readMaster bool
	}{
		{[]string{"master", "--version", "v4.1.22", "--yes"}, "/api/master/software-updates/master", false},
		{[]string{"agent", "--worker", "node-a", "--version", "4.1.22", "--yes"}, "/api/master/software-updates/agents/node-a", false},
		{[]string{"agent", "--worker", "2001:db8::1", "--yes"}, "/api/master/software-updates/agents/2001:db8::1", true},
		{[]string{"agents", "--all", "--yes"}, "/api/master/software-updates/agents", true},
	} {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			paths := []string{}
			client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				if r.Method == "GET" && test.readMaster && len(paths) == 1 {
					adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"master": map[string]interface{}{"info": map[string]interface{}{"version": "4.1.22"}}, "latest": map[string]interface{}{"version": "4.1.99"}}})
					return
				}
				if r.Method != "POST" || r.URL.Path != test.path {
					t.Fatalf("unexpected request %s %s", r.Method, r.URL)
				}
				if body := adminTestBody(t, r); !reflect.DeepEqual(body, map[string]interface{}{"version": "4.1.22"}) {
					t.Fatalf("wrong version %#v", body)
				}
				w.WriteHeader(http.StatusAccepted)
				adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"phase": "QUEUED", "version": "4.1.22", "id": "update-1"}})
			})
			result, err := runMaintenanceCommand(client, "update", test.args)
			wantCalls := 1
			if test.readMaster {
				wantCalls = 2
			}
			if err != nil || len(paths) != wantCalls || adminData(result)["phase"] != "QUEUED" {
				t.Fatalf("paths=%v result=%#v err=%v", paths, result, err)
			}
		})
	}
}

func TestMaintenanceMissingMasterVersionDoesNotUpgradeAgents(t *testing.T) {
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Fatal("missing version submitted an update")
		}
		adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"master": map[string]interface{}{"info": map[string]interface{}{"version": "development"}}}})
	})
	if _, err := runMaintenanceCommand(client, "update", []string{"agent", "--worker", "node-a", "--yes"}); err == nil {
		t.Fatal("accepted unknown master version")
	}
}

func TestMaintenanceServerRejectionsAreNotSuccess(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusConflict, http.StatusForbidden} {
		for _, cmd := range []struct {
			group string
			args  []string
		}{
			{"update", []string{"check"}}, {"update", []string{"master", "--version", "4.1.22", "--yes"}},
			{"update", []string{"agent", "--worker", "node-a", "--version", "4.1.22", "--yes"}}, {"backup", []string{"prune", "master", "--yes"}},
		} {
			client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				adminTestJSON(t, w, map[string]interface{}{"success": false, "message": "server rejected maintenance"})
			})
			result, err := runMaintenanceCommand(client, cmd.group, cmd.args)
			if err == nil || !strings.Contains(err.Error(), "server rejected maintenance") || result["success"] != false {
				t.Fatalf("rejection lost: %#v %v", result, err)
			}
		}
	}
}

func TestMaintenanceBackupTimeoutIsScopedAndCancellationPreserved(t *testing.T) {
	for _, args := range [][]string{{"list"}, {"prune", "master", "--yes"}, {"prune", "agent", "--worker", "node-a", "--yes"}} {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			calls++
			deadline, ok := r.Context().Deadline()
			left := time.Until(deadline)
			if calls == 1 {
				wantPath := "/api/master/software-updates/backups"
				wantMethod := "GET"
				if args[0] == "prune" {
					wantMethod = "POST"
					if args[1] == "master" {
						wantPath += "/master/prune"
					} else {
						wantPath += "/agents/node-a/prune"
					}
				}
				if r.Method != wantMethod || r.URL.Path != wantPath {
					t.Fatalf("wrong backup target: %s %s", r.Method, r.URL)
				}
				if !ok || left < 115*time.Second || left > 120*time.Second {
					t.Fatalf("backup deadline %v %v", ok, left)
				}
				if strings.HasSuffix(r.URL.Path, "/prune") && len(adminTestBody(t, r)) != 0 {
					t.Fatal("prune sent unexpected body")
				}
			} else if !ok || left > 60*time.Second {
				t.Fatalf("backup timeout leaked: %v", left)
			}
			adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"removedCount": 2, "freedBytes": 1024}})
		})
		client.client.Timeout = 60 * time.Second
		original := client.client
		client.requestContext = ctx
		result, err := runMaintenanceCommand(client, "backup", args)
		if err != nil || getInt(adminData(result), "removedCount") != 2 {
			t.Fatalf("result=%#v err=%v", result, err)
		}
		if client.client != original || client.client.Timeout != 60*time.Second {
			t.Fatal("shared client changed")
		}
		if _, err = client.Get("/software-updates"); err != nil {
			t.Fatal(err)
		}
		cancel()
		client.client.Transport = adminTestTransport(func(r *http.Request) (*http.Response, error) {
			if r.Context().Err() != context.Canceled {
				t.Fatal("backup request lost caller cancellation")
			}
			return nil, r.Context().Err()
		})
		if _, err = runMaintenanceCommand(client, "backup", []string{"list"}); err == nil {
			t.Fatal("canceled request succeeded")
		}
	}
}

func TestMaintenanceStatusShowsVersionSkewQueueAndOfflineHonestly(t *testing.T) {
	result := map[string]interface{}{"success": true, "data": map[string]interface{}{
		"master": map[string]interface{}{"info": map[string]interface{}{"version": "4.1.22"}},
		"agents": []interface{}{
			map[string]interface{}{"address": "agent-a", "displayName": "storage", "displayAddress": "10.0.0.2", "info": map[string]interface{}{"version": "4.1.21", "supported": true}},
			map[string]interface{}{"address": "agent-b", "displayName": "offline", "info": nil, "unavailable": true},
			map[string]interface{}{"address": "agent-c", "displayName": "ready", "info": map[string]interface{}{"version": "4.1.22", "supported": true}},
		},
		"queue":    map[string]interface{}{"phase": "RUNNING", "items": []interface{}{map[string]interface{}{"address": "agent-a", "phase": "QUEUED"}}},
		"checking": true, "checkError": "Previous check failed",
	}}
	var out bytes.Buffer
	if !renderMaintenanceResult(&out, result) {
		t.Fatal("maintenance status not rendered")
	}
	for _, want := range []string{"storage (10.0.0.2)", "4.1.21", "4.1.22", "QUEUED", "MISMATCH", "MATCH", "UNKNOWN", "OFFLINE", "RUNNING", "Previous check failed"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q: %s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "COMPLETED") {
		t.Fatalf("queued update called complete: %s", out.String())
	}
}

func TestAgentInviteStatusIsReadOnlyAndRejectsJoinLinks(t *testing.T) {
	for _, args := range [][]string{nil, {""}, {"id", "extra"}, {"https://example/api/agents/bootstrap/secret"}, {"../secret"}, {"id?token=secret"}, {"id%2Fsecret"}, {"token\n"}} {
		client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			t.Fatalf("invalid invitation performed request: %v", args)
		})
		if _, err := agentInviteStatus(client, args); err == nil {
			t.Fatalf("invalid invitation accepted %v", args)
		}
	}
	for _, state := range []string{"PENDING", "JOINED", "EXPIRED"} {
		client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" || r.URL.Path != "/api/master/agents/invitations/invite_123-x" {
				t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
			}
			adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"status": state}})
		})
		result, err := agentInviteStatus(client, []string{"invite_123-x"})
		if err != nil || adminData(result)["status"] != state {
			t.Fatalf("result=%#v err=%v", result, err)
		}
	}
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		adminTestJSON(t, w, map[string]interface{}{"success": false, "message": "Invalid or expired Agent invitation"})
	})
	if _, err := agentInviteStatus(client, []string{"unknown"}); err == nil {
		t.Fatal("unknown invitation reported success")
	}
}
