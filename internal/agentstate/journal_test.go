package agentstate

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/state"
)

func openJournal(t *testing.T, path string, o Options) (*state.Store, *Journal) {
	t.Helper()
	store, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	j, err := NewJournal(store, o)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	return store, j
}
func claim(t *testing.T, j *Journal, key string) {
	t.Helper()
	c, err := j.Begin(context.Background(), key, "fingerprint")
	if err != nil || !c.Acquired {
		t.Fatalf("claim: %+v %v", c, err)
	}
}
func TestCompletedAndInterruptedCommandsSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	store, j := openJournal(t, path, Options{})
	ctx := context.Background()
	claim(t, j, "completed")
	response := agenttransport.Response{Status: 201, Payload: json.RawMessage(`{"object":"saved"}`)}
	if err := j.Complete(ctx, "completed", "fingerprint", response); err != nil {
		t.Fatal(err)
	}
	claim(t, j, "interrupted")
	firstBoot := j.bootID
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, restarted := openJournal(t, path, Options{})
	defer store.Close()
	if firstBoot == restarted.bootID {
		t.Fatal("boot identity was reused")
	}
	// A lost HTTP acknowledgment replays the committed outcome without acquiring execution.
	saved, err := restarted.Begin(ctx, "completed", "fingerprint")
	if err != nil || saved.Acquired || saved.State != agenttransport.JournalCompleted || string(saved.Response.Payload) != string(response.Payload) {
		t.Fatalf("lost committed result: %+v %v", saved, err)
	}
	interrupted, err := restarted.Begin(ctx, "interrupted", "fingerprint")
	if err != nil || interrupted.Acquired || interrupted.State != agenttransport.JournalUnknown {
		t.Fatalf("unfinished command became executable: %+v %v", interrupted, err)
	}
	if err = restarted.Complete(ctx, "interrupted", "fingerprint", response); !errors.Is(err, ErrUnknownOutcome) {
		t.Fatalf("unknown outcome replaced: %v", err)
	}
	if _, err = restarted.Begin(ctx, "completed", "other payload"); !errors.Is(err, agenttransport.ErrIdempotencyConflict) {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, restarted = openJournal(t, path, Options{})
	defer store.Close()
	interrupted, err = restarted.Begin(ctx, "interrupted", "fingerprint")
	if err != nil || interrupted.State != agenttransport.JournalUnknown {
		t.Fatal("unknown state was not durable")
	}
}
func TestConcurrentBeginHasSingleWinner(t *testing.T) {
	store, j := openJournal(t, filepath.Join(t.TempDir(), "agent.db"), Options{})
	defer store.Close()
	start := make(chan struct{})
	var workers sync.WaitGroup
	var winners atomic.Int32
	for i := 0; i < 32; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			c, err := j.Begin(context.Background(), "same", "fingerprint")
			if err != nil {
				t.Error(err)
				return
			}
			if c.Acquired {
				winners.Add(1)
			}
		}()
	}
	close(start)
	workers.Wait()
	if winners.Load() != 1 {
		t.Fatalf("%d execution claims won", winners.Load())
	}
	s, err := j.Stats()
	if err != nil || s.Entries != 1 || s.ResponseBytes != 0 {
		t.Fatalf("bad accounting: %+v %v", s, err)
	}
}
func TestLimitsAndSerializationFailureAreAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	store, j := openJournal(t, path, Options{MaxEntries: 1, MaxResponseBytes: 8})
	defer store.Close()
	ctx := context.Background()
	claim(t, j, "one")
	if _, err := j.Begin(ctx, "two", "fingerprint"); !errors.Is(err, agenttransport.ErrJournalFull) {
		t.Fatal(err)
	}
	// Metadata is written first; invalid JSON makes the later record write fail.
	if err := j.Complete(ctx, "one", "fingerprint", agenttransport.Response{Status: 200, Payload: json.RawMessage(`{`)}); err == nil {
		t.Fatal("invalid response committed")
	}
	s, err := j.Stats()
	if err != nil || s.Entries != 1 || s.ResponseBytes != 0 {
		t.Fatalf("transaction partly committed: %+v %v", s, err)
	}
	current, err := j.Begin(ctx, "one", "fingerprint")
	if err != nil || current.Acquired || current.State != agenttransport.JournalRunning {
		t.Fatalf("claim changed on write failure: %+v %v", current, err)
	}
	if err = j.Complete(ctx, "one", "fingerprint", agenttransport.Response{Status: 200, Payload: json.RawMessage(`"too large"`)}); !errors.Is(err, agenttransport.ErrJournalFull) {
		t.Fatal(err)
	}
	response := agenttransport.Response{Status: 200, Payload: json.RawMessage(`{"a":1}`)}
	if err = j.Complete(ctx, "one", "fingerprint", response); err != nil {
		t.Fatal(err)
	}
	if err = j.Complete(ctx, "one", "fingerprint", response); err != nil {
		t.Fatal(err)
	}
	if err = j.Unknown(ctx, "one", "fingerprint"); err != nil {
		t.Fatal(err)
	}
	s, err = j.Stats()
	if err != nil || s.ResponseBytes != int64(len(response.Payload)) {
		t.Fatalf("duplicate completion changed quota: %+v %v", s, err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, j = openJournal(t, path, Options{MaxEntries: 1, MaxResponseBytes: 8})
	defer store.Close()
	s, err = j.Stats()
	if err != nil || s.Entries != 1 || s.ResponseBytes != int64(len(response.Payload)) {
		t.Fatal("quota was not durable")
	}
	if _, err = j.Begin(ctx, "two", "fingerprint"); !errors.Is(err, agenttransport.ErrJournalFull) {
		t.Fatal("restart forgot journal limit")
	}
}
func TestCancellationAndClosedStoreDoNotCreateClaims(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	store, j := openJournal(t, path, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := j.Begin(ctx, "cancelled", "fingerprint"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Begin(context.Background(), "closed", "fingerprint"); err == nil {
		t.Fatal("closed database accepted claim")
	}
	store, j = openJournal(t, path, Options{})
	defer store.Close()
	s, err := j.Stats()
	if err != nil || s.Entries != 0 {
		t.Fatalf("failed claim persisted: %+v %v", s, err)
	}
}
