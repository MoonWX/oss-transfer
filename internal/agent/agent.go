// Package agent runs the independent execution process and its outbound connection.
package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentstate"
	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/cluster"
	"github.com/MoonWX/oss-transfer/internal/state"
)

type Identity struct {
	InstanceID    string `json:"instanceId"`
	MasterURL     string `json:"masterUrl"`
	Credential    string `json:"credential"`
	EnrollmentID  string `json:"enrollmentId,omitempty"`
	PendingToken  string `json:"pendingToken,omitempty"`
	UsedTokenHash string `json:"usedTokenHash,omitempty"`
	Address       string `json:"address"`
	Label         string `json:"label"`
}
type Options struct {
	MasterURL     string
	JoinToken     string
	Address       string
	Label         string
	HTTPTransport http.RoundTripper
	Executor      agenttransport.Executor
	// OnReady is called after an authenticated Master poll succeeds.
	OnReady func() error
}
type Agent struct {
	store    *state.Store
	identity Identity
	options  Options
	bootID   string
}

func tokenHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func New(store *state.Store, options Options) (*Agent, error) {
	if err := store.ClaimRole("agent"); err != nil {
		return nil, err
	}
	var identity Identity
	err := store.Update(func(tx *state.Tx) error {
		found, err := tx.Get("agent-identity", "self", &identity)
		if err != nil {
			return err
		}
		if !found {
			if options.MasterURL == "" || options.JoinToken == "" {
				return errors.New("a new Agent requires --join-url with an invitation from Nodes > Add node")
			}
			identity.InstanceID, err = cluster.Token()
			if err != nil {
				return err
			}
			identity.MasterURL = strings.TrimRight(options.MasterURL, "/")
		} else if options.MasterURL != "" && strings.TrimRight(options.MasterURL, "/") != identity.MasterURL {
			return errors.New("Agent is enrolled with another Master; use a separate data directory")
		}
		u, err := url.Parse(identity.MasterURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("Master URL must be an HTTP or HTTPS origin")
		}
		if options.JoinToken != "" && tokenHash(options.JoinToken) != identity.UsedTokenHash && options.JoinToken != identity.PendingToken {
			identity.PendingToken = options.JoinToken
			identity.Credential, err = cluster.Token()
			if err != nil {
				return err
			}
			identity.EnrollmentID = ""
			identity.Address = options.Address
			identity.Label = options.Label
			if identity.Label == "" {
				identity.Label, _ = os.Hostname()
			}
		}
		if identity.Credential == "" || (identity.EnrollmentID == "" && identity.PendingToken == "") {
			return errors.New("Agent identity is incomplete; refusing to generate a replacement identity")
		}
		return tx.Put("agent-identity", "self", identity)
	})
	if err != nil {
		return nil, err
	}
	bootID, err := cluster.Token()
	if err != nil {
		return nil, err
	}
	return &Agent{store: store, identity: identity, options: options, bootID: bootID}, nil
}

// Enroll keeps the identity and secret durable before the first request. A lost
// join response is retried with exactly the same identity after restart.
func (a *Agent) Enroll(ctx context.Context) error {
	if a.identity.EnrollmentID != "" {
		return nil
	}
	data, err := json.Marshal(cluster.JoinRequest{Token: a.identity.PendingToken, InstanceID: a.identity.InstanceID, Credential: a.identity.Credential, Address: a.identity.Address, Label: a.identity.Label})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, "POST", a.identity.MasterURL+"/api/agents/join", bytes.NewReader(data))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	client := http.Client{Transport: a.options.HTTPTransport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("enroll Agent: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return fmt.Errorf("enroll Agent: HTTP %d", response.StatusCode)
	}
	var result struct {
		Success bool           `json:"success"`
		Data    cluster.Member `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 32*1024)).Decode(&result); err != nil {
		return fmt.Errorf("invalid enrollment response: %w", err)
	}
	if !result.Success || result.Data.ID != a.identity.InstanceID || result.Data.EnrollmentID == "" {
		return errors.New("Master returned an inconsistent enrollment")
	}
	next := a.identity
	next.EnrollmentID = result.Data.EnrollmentID
	next.Address = result.Data.Address
	next.Label = result.Data.Label
	next.UsedTokenHash = tokenHash(next.PendingToken)
	next.PendingToken = ""
	if err := a.store.Update(func(tx *state.Tx) error { return tx.Put("agent-identity", "self", next) }); err != nil {
		return err
	}
	a.identity = next
	return nil
}
func (a *Agent) Session() agenttransport.Session {
	return agenttransport.Session{NodeID: a.identity.InstanceID, EnrollmentID: a.identity.EnrollmentID}
}
func (a *Agent) Run(ctx context.Context) error {
	if err := a.Enroll(ctx); err != nil {
		return err
	}
	transport, err := agenttransport.NewHTTPTransport(a.identity.MasterURL, a.identity.Credential, a.options.HTTPTransport, 0)
	if err != nil {
		return err
	}
	journal, err := agentstate.NewJournal(a.store, agentstate.Options{})
	if err != nil {
		return err
	}
	executor := runtimeExecutor{agent: a}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	readyFailures := make(chan error, 1)
	connected := &readyTransport{Transport: transport, ready: a.options.OnReady, failed: func(err error) { readyFailures <- err; cancel() }}
	err = agenttransport.NewRunner(connected, executor, journal, agenttransport.RunnerOptions{}).Run(ctx)
	select {
	case failed := <-readyFailures:
		return failed
	default:
		return err
	}
}

type runtimeExecutor struct{ agent *Agent }

func (e runtimeExecutor) ReplaySafe(operation string) bool {
	if delegate, ok := e.agent.options.Executor.(agenttransport.ReplaySafeExecutor); ok {
		return delegate.ReplaySafe(operation)
	}
	return false
}
func (e runtimeExecutor) Execute(ctx context.Context, command agenttransport.Command) (agenttransport.Response, error) {
	a := e.agent
	if command.Session != a.Session() {
		return agenttransport.Response{}, agenttransport.ErrSession
	}
	if command.Operation == "capabilities" {
		hostname, err := os.Hostname()
		if err != nil {
			return agenttransport.Response{}, err
		}
		payload, err := json.Marshal(map[string]any{"nodeId": a.identity.InstanceID, "bootId": a.bootID, "hostname": hostname, "os": runtime.GOOS, "arch": runtime.GOARCH, "protocolVersion": 1})
		return agenttransport.Response{Status: 200, Payload: payload}, err
	}
	if a.options.Executor != nil {
		return a.options.Executor.Execute(ctx, command)
	}
	return agenttransport.Response{Status: 501, Payload: json.RawMessage(`{"code":"unsupported_operation"}`)}, nil
}

// Readiness means the current credential was accepted, including an empty long
// poll. Merely opening the database or restoring an old enrollment is not proof.
type readyTransport struct {
	agenttransport.Transport
	once   sync.Once
	ready  func() error
	failed func(error)
}

func (t *readyTransport) Poll(ctx context.Context) (agenttransport.Command, error) {
	command, err := t.Transport.Poll(ctx)
	if err == nil || errors.Is(err, agenttransport.ErrNoCommand) {
		t.once.Do(func() {
			if t.ready != nil {
				if err := t.ready(); err != nil {
					t.failed(err)
				}
			}
		})
	}
	return command, err
}
