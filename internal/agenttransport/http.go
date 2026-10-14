package agenttransport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const PollPath = "/api/agents/poll"
const ResultPath = "/api/agents/result"

var ErrNoCommand = errors.New("no agent command available")

type Authenticate func(*http.Request) (Session, error)

// HTTPHandler authenticates each request; enrollment state is rechecked by the
// broker after long polling. The caller mounts it only at PollPath and ResultPath.
type HTTPHandler struct {
	Broker       *Broker
	Authenticate Authenticate
	PollDuration time.Duration
}

func (h HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if h.Broker == nil || h.Authenticate == nil {
		http.Error(w, "agent transport unavailable", 503)
		return
	}
	session, err := h.Authenticate(r)
	if err != nil {
		http.Error(w, "invalid agent credential", 401)
		return
	}
	switch {
	case r.URL.Path == PollPath && r.Method == http.MethodGet:
		wait := h.PollDuration
		if wait <= 0 || wait > 25*time.Second {
			wait = 25 * time.Second
		}
		ctx, cancel := context.WithTimeout(r.Context(), wait)
		defer cancel()
		c, err := h.Broker.poll(ctx, session, true)
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			w.WriteHeader(204)
			return
		}
		if errors.Is(err, ErrClosed) {
			http.Error(w, "agent transport unavailable", 503)
			return
		}
		if err != nil {
			http.Error(w, "agent enrollment unavailable", 401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(c)
	case r.URL.Path == PollPath && r.Method == http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 1024)
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		var receipt Receipt
		if err := dec.Decode(&receipt); err != nil || receipt.RequestID == "" || len(receipt.RequestID) > 256 || receipt.DeliveryID == "" || len(receipt.DeliveryID) > 256 {
			http.Error(w, "invalid command receipt", 400)
			return
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			http.Error(w, "invalid command receipt", 400)
			return
		}
		switch err := h.Broker.Confirm(session, receipt); {
		case err == nil:
			w.WriteHeader(204)
		case errors.Is(err, ErrSession):
			http.Error(w, "agent enrollment unavailable", 401)
		case errors.Is(err, ErrClosed):
			http.Error(w, "agent transport unavailable", 503)
		default:
			http.Error(w, "command delivery expired", 409)
		}
	case r.URL.Path == ResultPath && r.Method == http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, int64(h.Broker.options.MaxPayloadBytes)+64*1024)
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		var result Result
		if err := dec.Decode(&result); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "command result too large", 413)
				return
			}
			http.Error(w, "invalid command result", 400)
			return
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			http.Error(w, "invalid command result", 400)
			return
		}
		err := h.Broker.Complete(session, result)
		switch {
		case err == nil || errors.Is(err, ErrUnknownRequest):
			w.WriteHeader(204)
		case errors.Is(err, ErrSession):
			http.Error(w, "agent enrollment unavailable", 401)
		case errors.Is(err, ErrPayloadTooLarge):
			http.Error(w, "command result too large", 413)
		default:
			http.Error(w, "invalid command result", 400)
		}
	default:
		http.NotFound(w, r)
	}
}

// HTTPTransport owns a client that does not forward credentials across redirects.
// A caller may provide an http.RoundTripper for TLS/proxy configuration.
type HTTPTransport struct {
	origin, credential string
	client             *http.Client
	maxPayload         int
}

func NewHTTPTransport(origin, credential string, rt http.RoundTripper, maxPayload int) (*HTTPTransport, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, ErrInvalidRequest
	}
	if credential == "" || strings.ContainsAny(credential, "\r\n") {
		return nil, ErrInvalidRequest
	}
	if maxPayload <= 0 {
		maxPayload = 8 << 20
	}
	return &HTTPTransport{origin: strings.TrimRight(origin, "/"), credential: credential, maxPayload: maxPayload, client: &http.Client{Transport: rt, Timeout: 35 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (t *HTTPTransport) request(ctx context.Context, path, method string, body []byte) (*http.Response, error) {
	r, err := http.NewRequestWithContext(ctx, method, t.origin+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	r.Header.Set("X-Agent-Key", t.credential)
	r.Header.Set("Content-Type", "application/json")
	return t.client.Do(r)
}
func (t *HTTPTransport) Poll(ctx context.Context) (Command, error) {
	resp, err := t.request(ctx, PollPath, http.MethodGet, nil)
	if err != nil {
		return Command{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 204 {
		return Command{}, ErrNoCommand
	}
	if resp.StatusCode == 401 {
		return Command{}, ErrSession
	}
	if resp.StatusCode != 200 {
		return Command{}, fmt.Errorf("agent poll returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(t.maxPayload)+64*1024+1))
	if err != nil {
		return Command{}, err
	}
	if len(body) > t.maxPayload+64*1024 {
		return Command{}, ErrPayloadTooLarge
	}
	var c Command
	if err = json.Unmarshal(body, &c); err != nil {
		return Command{}, ErrInvalidRequest
	}
	if len(c.Payload) > t.maxPayload {
		return Command{}, ErrPayloadTooLarge
	}
	// A poll response may be lost when a previous runner disconnects. Execution
	// starts only after the broker has accepted this particular delivery token.
	body, err = json.Marshal(Receipt{RequestID: c.RequestID, DeliveryID: c.DeliveryID})
	if err != nil {
		return Command{}, err
	}
	ack, err := t.request(ctx, PollPath, http.MethodPost, body)
	if err != nil {
		return Command{}, err
	}
	defer ack.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(ack.Body, 4096))
	if ack.StatusCode == 401 {
		return Command{}, ErrSession
	}
	if ack.StatusCode == 409 {
		return Command{}, ErrNoCommand
	}
	if ack.StatusCode != 204 {
		return Command{}, fmt.Errorf("agent receipt returned HTTP %d", ack.StatusCode)
	}
	return c, nil
}
func (t *HTTPTransport) Submit(ctx context.Context, result Result) error {
	if len(result.Response.Payload) > t.maxPayload {
		return ErrPayloadTooLarge
	}
	body, err := json.Marshal(result)
	if err != nil {
		return err
	}
	resp, err := t.request(ctx, ResultPath, http.MethodPost, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == 401 {
		return ErrSession
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("agent result returned HTTP %d", resp.StatusCode)
	}
	return nil
}
