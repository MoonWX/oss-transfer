package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
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
