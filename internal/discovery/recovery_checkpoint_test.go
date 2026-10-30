package discovery

import (
	"context"
	"encoding/xml"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func TestTakeoverCheckpointLocalRetainsOnlyAssignedPageBoundary(t *testing.T) {
	for _, assigned := range []int64{1, 2} {
		t.Run(string(rune('0'+assigned)), func(t *testing.T) {
			ctx := context.Background()
			root := checkpointSource(t)
			old, _ := newScanner(t, filepath.Join(t.TempDir(), "old.db"), &checkpointLocalFactory{}, Options{})
			if _, err := old.Start(ctx, localSpec(root)); err != nil {
				t.Fatal(err)
			}
			oldState := waitStatus(t, old, Completed)
			if oldState.TotalObjects != 260 || oldState.LastBatch != 3 {
				t.Fatal(oldState)
			}
			var batches []Batch
			for n := int64(0); n < assigned; n++ {
				batch, found, err := old.Batch(ctx, 1, 1, n)
				if err != nil || !found {
					t.Fatalf("read assigned batch: %v %v", found, err)
				}
				batches = append(batches, batch)
			}
			checkpoint := batches[0].Checkpoint
			if checkpoint == nil || checkpoint.ThroughBatch != 1 || checkpoint.Objects != 128 || checkpoint.SourceDone {
				t.Fatalf("assignment carried scan-ahead instead of the completed page: %+v", checkpoint)
			}
			if oldState.TransferCheckpoint.ThroughBatch != 3 {
				t.Fatal("old scan did not run ahead")
			}
			// Simulate another mount path to the same volume. The source scanner
			// is gone and its unassigned inventory is not copied to the new Agent.
			alias := filepath.Join(t.TempDir(), "shared-mount")
			if err := os.Symlink(root, alias); err != nil {
				t.Skipf("directory alias unavailable: %v", err)
			}
			if err := os.WriteFile(filepath.Join(root, "a-done", "0000.bin"), []byte("changed behind the completed cursor"), 0600); err != nil {
				t.Fatal(err)
			}
			factory := &checkpointLocalFactory{}
			path := filepath.Join(t.TempDir(), "new.db")
			fresh, db := newScanner(t, path, factory, Options{})
			request := takeoverRequest(localSpec(alias), batches...)
			request.Checkpoint = checkpoint
			seedTakeover(t, fresh, request)
			// Recovery itself may restart after the import manifest is committed.
			if err := fresh.Close(); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			fresh, _ = newScanner(t, path, factory, Options{})
			if _, err := fresh.FinalizeTakeover(ctx, 1, 1, 1); err != nil {
				t.Fatal(err)
			}
			done := waitStatus(t, fresh, Completed)
			if done.TotalObjects != 260 || done.SeenObjects != 260 || done.TotalBytes != 260*9 || done.MissingObjects != 0 || done.Warning != "" {
				t.Fatalf("takeover skipped unassigned objects or duplicated assigned objects: %+v", done)
			}
			cursors, inspected := factory.observed()
			if len(cursors) != 1 || cursors[0] == "" || cursors[0] == checkpoint.Cursor || len(inspected) != 132 {
				t.Fatalf("shared cursor was not rebound/resumed: cursors=%v inspected=%d", cursors, len(inspected))
			}
			for _, key := range inspected {
				if !strings.HasPrefix(key, "z-rest/") {
					t.Fatalf("revisited completed subtree: %s", key)
				}
			}
			keys := takeoverNewKeys(t, fresh, request)
			want := 132
			if assigned == 2 {
				want = 4
			}
			if len(keys) != want {
				t.Fatalf("new work=%d want=%d", len(keys), want)
			}
		})
	}
}

func TestTakeoverCheckpointS3UsesProviderContinuationAfterAssignedPage(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var tokens []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("continuation-token")
		mu.Lock()
		tokens = append(tokens, token)
		mu.Unlock()
		if r.Method != "GET" || r.URL.Query().Get("list-type") != "2" {
			t.Error("scanner read object body")
			w.WriteHeader(400)
			return
		}
		page := checkpointHTTPPage{}
		switch token {
		case "":
			page = checkpointHTTPPage{IsTruncated: true, NextContinuationToken: "opaque/+next=", Contents: []checkpointHTTPObject{{Key: "z-first", Size: 1, ETag: "first", LastModified: "2026-10-29T00:00:00Z"}}}
		case "opaque/+next=":
			page.Contents = []checkpointHTTPObject{{Key: "a-tail", Size: 2, ETag: "tail", LastModified: "2026-10-29T00:00:00Z"}, {Key: "m-tail", Size: 3, ETag: "tail", LastModified: "2026-10-29T00:00:00Z"}}
		default:
			t.Errorf("unexpected token %q", token)
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		if err := xml.NewEncoder(w).Encode(page); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	factory := checkpointHTTPFactory{server: server}
	spec := Spec{ParentID: 1, Generation: 1, Source: execution.Endpoint{Kind: "S3", Ref: "saved", Bucket: "bucket"}}
	old, _ := newScanner(t, filepath.Join(t.TempDir(), "old.db"), factory, Options{})
	if _, err := old.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, old, Completed)
	batch, found, err := old.Batch(ctx, 1, 1, 0)
	if err != nil || !found || batch.Checkpoint == nil || batch.Checkpoint.SourceDone {
		t.Fatalf("first page checkpoint: %+v %v", batch, err)
	}
	request := takeoverRequest(spec, batch)
	request.Checkpoint = batch.Checkpoint
	fresh, _ := newScanner(t, filepath.Join(t.TempDir(), "new.db"), factory, Options{})
	seedTakeover(t, fresh, request)
	mu.Lock()
	tokens = nil
	mu.Unlock()
	if _, err := fresh.FinalizeTakeover(ctx, 1, 1, 1); err != nil {
		t.Fatal(err)
	}
	done := waitStatus(t, fresh, Completed)
	if done.TotalObjects != 3 || done.TotalBytes != 6 || done.SeenObjects != 3 || done.MissingObjects != 0 {
		t.Fatal(done)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(tokens, []string{"opaque/+next="}) {
		t.Fatalf("takeover restarted S3 listing: %v", tokens)
	}
}

func TestTakeoverRejectsCheckpointAheadOfAssignedManifest(t *testing.T) {
	scanner, _ := newScanner(t, filepath.Join(t.TempDir(), "new.db"), &checkpointLocalFactory{}, Options{})
	request := takeoverRequest(localSpec(checkpointSource(t)))
	request.Checkpoint = &RecoveryCheckpoint{Version: 1, Sequence: 1, ThroughBatch: 1, Objects: 1, Bytes: 9, Cursor: "unassigned"}
	if _, err := scanner.SeedTakeover(context.Background(), request); !errors.Is(err, ErrInvalid) {
		t.Fatalf("accepted checkpoint that can skip unassigned files: %v", err)
	}
}

func TestTakeoverCheckpointS3ContinuesAfterEntireFilteredPage(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var tokens []string
	blocked := make(chan struct{})
	first := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("continuation-token")
		mu.Lock()
		tokens = append(tokens, token)
		block := token == "filtered-page-complete" && first
		if block {
			first = false
		}
		mu.Unlock()
		if block {
			close(blocked)
			<-r.Context().Done()
			return
		}
		page := checkpointHTTPPage{}
		switch token {
		case "":
			page = checkpointHTTPPage{IsTruncated: true, NextContinuationToken: "filtered-page-complete", Contents: []checkpointHTTPObject{{Key: "old", Size: 1, LastModified: "2026-10-01T00:00:00Z"}}}
		case "filtered-page-complete":
			page.Contents = []checkpointHTTPObject{{Key: "new", Size: 2, LastModified: "2026-10-29T00:00:00Z"}}
		default:
			t.Errorf("unexpected token %q", token)
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		if err := xml.NewEncoder(w).Encode(page); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	factory := checkpointHTTPFactory{server: server}
	spec := Spec{ParentID: 1, Generation: 1, Source: execution.Endpoint{Kind: "S3", Ref: "saved", Bucket: "bucket"}, ModifiedFrom: time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)}
	old, _ := newScanner(t, filepath.Join(t.TempDir(), "old.db"), factory, Options{})
	if _, err := old.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	waitReached(t, blocked)
	state := waitS3Checkpoint(t, old)
	if state.TotalObjects != 0 || state.TransferCheckpoint == nil || state.TransferCheckpoint.ThroughBatch != 0 {
		t.Fatal(state)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	request := takeoverRequest(spec)
	request.Checkpoint = state.TransferCheckpoint
	fresh, _ := newScanner(t, filepath.Join(t.TempDir(), "new.db"), factory, Options{})
	seedTakeover(t, fresh, request)
	mu.Lock()
	tokens = nil
	mu.Unlock()
	if _, err := fresh.FinalizeTakeover(ctx, 1, 1, 1); err != nil {
		t.Fatal(err)
	}
	done := waitStatus(t, fresh, Completed)
	if done.TotalObjects != 1 || done.TotalBytes != 2 || done.SeenObjects != 1 {
		t.Fatal(done)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(tokens, []string{"filtered-page-complete"}) {
		t.Fatalf("filtered prefix was rescanned: %v", tokens)
	}
}

func TestTakeoverCheckpointCompletedSourceDoesNotReopenAndUsesNewEpoch(t *testing.T) {
	factory := &unopenedCheckpointFactory{}
	scanner, _ := newScanner(t, filepath.Join(t.TempDir(), "new.db"), factory, Options{})
	request := takeoverRequest(localSpec(t.TempDir()), takeoverBatch(1, transferstore.Object{Key: "finished", Size: 9}))
	request.Checkpoint = &RecoveryCheckpoint{Version: 1, Sequence: 9, ThroughBatch: 1, Objects: 1, Bytes: 9, SourceDone: true}
	seedTakeover(t, scanner, request)
	if _, err := scanner.FinalizeTakeover(context.Background(), 1, 1, 1); err != nil {
		t.Fatal(err)
	}
	done := waitStatus(t, scanner, Completed)
	if factory.opened || done.TotalObjects != 1 || done.SeenObjects != 1 || done.TransferCheckpoint == nil || done.TransferCheckpoint.Epoch != 1 || !done.TransferCheckpoint.SourceDone {
		t.Fatal(done, factory.opened)
	}
}
