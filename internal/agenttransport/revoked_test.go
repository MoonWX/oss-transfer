package agenttransport

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A definitively rejected credential is reported as ErrRevoked so the Agent
// service stops; any other authentication failure stays a retryable ErrSession.
func TestHTTPRejectedCredentialReportsRevocation(t *testing.T) {
	b := broker(t, BrokerOptions{})
	server := httptest.NewServer(HTTPHandler{Broker: b, Authenticate: func(r *http.Request) (Session, error) {
		switch r.Header.Get("X-Agent-Key") {
		case "removed":
			return Session{}, fmt.Errorf("%w: removed", ErrCredentialRejected)
		case "unavailable":
			return Session{}, errors.New("registry unavailable")
		}
		return Session{}, ErrSession
	}})
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, test := range []struct {
		key     string
		revoked bool
	}{{"removed", true}, {"unavailable", false}, {"unknown-session", false}} {
		client, err := NewHTTPTransport(server.URL, test.key, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Poll(ctx)
		if !errors.Is(err, ErrSession) || errors.Is(err, ErrRevoked) != test.revoked {
			t.Fatalf("%s: poll error %v, revoked %v", test.key, err, test.revoked)
		}
		err = client.Submit(ctx, Result{DeliveryID: "d", RequestID: "r", Response: Response{Status: 200}})
		if !errors.Is(err, ErrSession) || errors.Is(err, ErrRevoked) != test.revoked {
			t.Fatalf("%s: submit error %v, revoked %v", test.key, err, test.revoked)
		}
	}
}

func TestRunnerReturnsRevocation(t *testing.T) {
	r := NewRunner(revokedTransport{}, ExecutorFunc(func(context.Context, Command) (Response, error) {
		return Response{Status: 200}, nil
	}), nil, RunnerOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.Run(ctx); !errors.Is(err, ErrRevoked) {
		t.Fatal("runner did not report the revocation", err)
	}
}

type revokedTransport struct{}

func (revokedTransport) Poll(context.Context) (Command, error) { return Command{}, ErrRevoked }
func (revokedTransport) Submit(context.Context, Result) error  { return ErrRevoked }
