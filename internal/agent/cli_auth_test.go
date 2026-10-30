package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/state"
)

func TestLocalCLIAuthRetriesUnavailableMasterAndPublishesOnce(t *testing.T) {
	var requests, publications atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/agents/cli-access" || r.Header.Get("X-Agent-Key") != "execution-credential" {
			t.Error("incorrect grant request")
		}
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusNotFound) // An older Master during a rolling upgrade.
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"success": true, "token": "dedicated-admin-grant"})
	}))
	defer server.Close()
	a := &Agent{identity: Identity{MasterURL: server.URL, EnrollmentID: "enrollment", Credential: "execution-credential"}, options: Options{OnCLIAuth: func(token string) error {
		if token != "dedicated-admin-grant" {
			t.Error("execution identity exported instead of dedicated grant")
		}
		publications.Add(1)
		return nil
	}}}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	a.publishCLIAuth(ctx)
	if requests.Load() != 2 || publications.Load() != 1 || ctx.Err() != nil {
		t.Fatal("grant not retried and published once", requests.Load(), publications.Load(), ctx.Err())
	}
}

func TestLocalCLIAuthDoesNotFollowRedirectOrExposeResponse(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer target.Close()
	for _, response := range []string{"redirect", "malformed", "execution-credential", "extra-json"} {
		t.Run(response, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch response {
				case "redirect":
					http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
				case "malformed":
					w.Write([]byte(`{"token":"secret-response-body",`))
				case "execution-credential":
					json.NewEncoder(w).Encode(map[string]any{"success": true, "token": "execution-credential"})
				case "extra-json":
					w.Write([]byte(`{"success":true,"token":"secret-response-body"}{}`))
				}
			}))
			defer server.Close()
			a := &Agent{identity: Identity{MasterURL: server.URL, EnrollmentID: "enrollment", Credential: "execution-credential"}}
			token, err := a.fetchCLIAuth(context.Background())
			if err == nil || token != "" || strings.Contains(err.Error(), "secret-response-body") || strings.Contains(err.Error(), "execution-credential") {
				t.Fatal("invalid response accepted or leaked")
			}
		})
	}
	if redirected.Load() != 0 {
		t.Fatal("Agent credential followed a redirect")
	}
}

func TestLocalCLIAuthRequestDoesNotBlockReadinessOrShutdown(t *testing.T) {
	accessStarted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/agents/cli-access":
			close(accessStarted)
			<-r.Context().Done()
		case agenttransport.PollPath:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	db, err := state.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	identity := Identity{InstanceID: "node", EnrollmentID: "enrollment", MasterURL: server.URL, Credential: "execution-credential"}
	if err := db.Update(func(tx *state.Tx) error { return tx.Put("agent-identity", "self", identity) }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ready := make(chan struct{})
	a, err := New(db, Options{OnCLIAuth: func(string) error { return errors.New("publication must not run without response") }, OnReady: func() error {
		close(ready)
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	select {
	case <-accessStarted:
	case <-ctx.Done():
		t.Fatal("grant request did not start")
	}
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("grant request blocked readiness")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("grant request blocked shutdown")
	}
}
