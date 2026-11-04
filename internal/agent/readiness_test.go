package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/state"
)

func TestReadinessRequiresSuccessfulAuthenticatedPoll(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusServiceUnavailable, http.StatusNoContent} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Agent-Key") != "qa-credential" {
					t.Error("poll omitted credential")
				}
				w.WriteHeader(status)
			}))
			defer server.Close()
			transport, err := agenttransport.NewHTTPTransport(server.URL, "qa-credential", nil, 1024)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			wrapped := readyTransport{Transport: transport, ready: func() error { calls++; return nil }, failed: func(err error) { t.Fatal(err) }}
			for i := 0; i < 2; i++ {
				_, _ = wrapped.Poll(context.Background())
			}
			want := 0
			if status == http.StatusNoContent {
				want = 1
			}
			if calls != want {
				t.Fatalf("readiness calls=%d, want=%d", calls, want)
			}
		})
	}
}

func TestReadinessWriteFailureIsPropagatedOnce(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer server.Close()
	transport, err := agenttransport.NewHTTPTransport(server.URL, "qa-credential", nil, 1024)
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("readiness disk full")
	calls := 0
	wrapped := readyTransport{Transport: transport, ready: func() error { return failure }, failed: func(err error) {
		calls++
		if !errors.Is(err, failure) {
			t.Fatal(err)
		}
	}}
	for i := 0; i < 2; i++ {
		_, _ = wrapped.Poll(context.Background())
	}
	if calls != 1 {
		t.Fatal("readiness failure was lost or repeated", calls)
	}
}

// The Master holds an idle poll for up to 25 s. Readiness must come from the
// first authenticated round trip, never from waiting that hold out.
func TestReadinessDoesNotWaitOutIdleLongPoll(t *testing.T) {
	session := agenttransport.Session{NodeID: "node", EnrollmentID: "enrollment"}
	broker := agenttransport.NewBroker(agenttransport.BrokerOptions{})
	defer broker.Close()
	if err := broker.Activate(session); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(agenttransport.HTTPHandler{Broker: broker, Authenticate: func(r *http.Request) (agenttransport.Session, error) {
		if r.Header.Get("X-Agent-Key") != "execution-credential" {
			return agenttransport.Session{}, agenttransport.ErrSession
		}
		return session, nil
	}})
	defer server.Close()
	db, err := state.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	identity := Identity{InstanceID: session.NodeID, EnrollmentID: session.EnrollmentID, MasterURL: server.URL, Credential: "execution-credential"}
	if err := db.Update(func(tx *state.Tx) error { return tx.Put("agent-identity", "self", identity) }); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	a, err := New(db, Options{OnReady: func() error { close(ready); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := time.Now()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	guard := time.After(30 * time.Second) // hang guard only
	for waiting := true; waiting; {
		// Readiness fires before the next poll starts, so a held poll seen
		// while readiness is still pending can only be the first one.
		if broker.Stats().Waiting > 0 {
			select {
			case <-ready:
				waiting = false
				continue
			default:
				t.Fatal("readiness waited on a held poll")
			}
		}
		select {
		case <-ready:
			waiting = false
		case err := <-done:
			t.Fatal("Agent stopped before readiness:", err)
		case <-guard:
			t.Fatal("Agent never became ready")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	t.Logf("time to ready: %v", time.Since(started))
	for broker.Stats().Waiting != 1 {
		select {
		case err := <-done:
			t.Fatal("Agent stopped after readiness:", err)
		case <-guard:
			t.Fatal("Agent did not resume long polling")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
