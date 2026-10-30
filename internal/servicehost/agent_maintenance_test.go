package servicehost

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func maintenanceConfig(t *testing.T, origin string) Config {
	t.Helper()
	c := Config{DataDirectory: t.TempDir()}
	if err := writeCLILocalAuth(filepath.Join(c.DataDirectory, "cli-local-auth.json"), "agent", origin, strings.Repeat("private-fixture-token", 3)); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAgentUpgradeMaintenanceUsesSameOperationWithoutOpeningDatabase(t *testing.T) {
	var operation string
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/master/agents/maintenance" || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("private-fixture-token", 3) {
			t.Error("incorrect authenticated maintenance request")
		}
		var request struct{ Action, ID string }
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if operation == "" {
			operation = request.ID
		} else if operation != request.ID {
			t.Error("end did not reference its own begin")
		}
		calls = append(calls, request.Action)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "id": request.ID, "expiresAt": time.Now().Add(30 * time.Minute)})
	}))
	defer server.Close()
	c := maintenanceConfig(t, server.URL)
	// Deliberately not a bbolt database: the running service owns that file.
	if err := os.WriteFile(filepath.Join(c.DataDirectory, "agent.db"), []byte("database must not be opened"), 0600); err != nil {
		t.Fatal(err)
	}
	receipt := filepath.Join(t.TempDir(), "receipt.json")
	for _, action := range []string{"begin", "begin", "end"} {
		if err := AgentUpgradeMaintenance(context.Background(), c, action, receipt); err != nil {
			t.Fatal(err)
		}
	}
	if len(calls) != 3 || strings.Join(calls, ",") != "begin,begin,end" {
		t.Fatal(calls)
	}
	if raw, err := os.ReadFile(receipt); err != nil || strings.Contains(string(raw), "private-fixture-token") {
		t.Fatal("receipt contains credential or unreadable", err)
	}
}

func TestAgentUpgradeMaintenanceRejectsOldMasterAndRedirects(t *testing.T) {
	var leaked atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer target.Close()
	for _, status := range []int{http.StatusNotFound, http.StatusUnauthorized, http.StatusTemporaryRedirect} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", target.URL)
				w.WriteHeader(status)
				fmt.Fprint(w, "untrusted private response")
			}))
			defer server.Close()
			c := maintenanceConfig(t, server.URL)
			err := AgentUpgradeMaintenance(context.Background(), c, "begin", filepath.Join(t.TempDir(), "receipt.json"))
			if err == nil || strings.Contains(err.Error(), "untrusted private") {
				t.Fatal("failed request accepted or leaked response", err)
			}
			if status == 404 && !strings.Contains(err.Error(), "upgrade the Master") {
				t.Fatal("missing old-Master guidance", err)
			}
		})
	}
	if leaked.Load() != 0 {
		t.Fatal("maintenance followed a credential redirect")
	}
}

func TestAgentUpgradeMaintenanceRejectsInvalidFilesAndAcknowledgements(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"success":true,"id":"wrong","expiresAt":"2026-10-30T10:00:00Z"}`)
	}))
	defer server.Close()
	c := maintenanceConfig(t, server.URL)
	if err := AgentUpgradeMaintenance(context.Background(), c, "begin", filepath.Join(t.TempDir(), "receipt.json")); err == nil {
		t.Fatal("incorrect operation acknowledgement accepted")
	}
	before := calls.Load()
	if err := AgentUpgradeMaintenance(context.Background(), c, "end", filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("end created a new operation")
	}
	if goruntime.GOOS != "windows" {
		if err := os.Chmod(filepath.Join(c.DataDirectory, "cli-local-auth.json"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := AgentUpgradeMaintenance(context.Background(), c, "begin", filepath.Join(t.TempDir(), "receipt.json")); err == nil {
			t.Fatal("public credential file accepted")
		}
	}
	if calls.Load() != before {
		t.Fatal("invalid local state contacted the Master")
	}
}
