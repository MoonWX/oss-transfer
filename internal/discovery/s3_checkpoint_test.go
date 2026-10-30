package discovery

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

type checkpointHTTPFactory struct {
	server *httptest.Server
}

func (f checkpointHTTPFactory) OpenEndpoint(ctx context.Context, endpoint execution.Endpoint) (transferstore.Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Return the concrete storage implementation so discovery exercises its
	// actual CheckpointScanner path, including SDK XML and token handling.
	return transferstore.NewS3(transferstore.S3Config{Endpoint: f.server.URL, Region: "test", Bucket: endpoint.Bucket,
		AccessKey: "test-access", SecretKey: "test-secret", PathStyle: true, RetryMaxAttempts: 1, HTTPClient: f.server.Client()})
}

type checkpointHTTPObject struct {
	Key          string
	Size         int64
	ETag         string
	LastModified string
}

type checkpointHTTPPage struct {
	XMLName               xml.Name `xml:"ListBucketResult"`
	IsTruncated           bool
	NextContinuationToken string `xml:",omitempty"`
	Contents              []checkpointHTTPObject
}

func waitS3Checkpoint(t *testing.T, scanner *Scanner) State {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, err := scanner.Status(context.Background(), 1)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status == Failed {
			t.Fatalf("scan failed before the checkpoint: %s", current.Error)
		}
		if current.Checkpoint != nil && current.Checkpoint.Cursor != "" {
			return current
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("first S3 page was not durably checkpointed")
	return State{}
}

func TestS3DiscoveryPersistsPageCheckpointAcrossDatabaseReopen(t *testing.T) {
	for _, test := range []struct {
		name          string
		filteredFirst bool
		rejectResume  bool
	}{
		{name: "objects"},
		{name: "whole-page-filtered", filteredFirst: true},
		{name: "rejected-token-stops", rejectResume: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			var mu sync.Mutex
			var requests []string
			gate := atomic.Bool{}
			gate.Store(true)
			reached := make(chan struct{})
			firstDate := "2026-10-29T00:00:00Z"
			if test.filteredFirst {
				firstDate = "2026-10-20T00:00:00Z"
			}
			firstObjects := make([]checkpointHTTPObject, 129)
			for index := range firstObjects {
				// Non-key order ensures continuation is based on the provider's
				// opaque token, not a last-Key assumption.
				firstObjects[index] = checkpointHTTPObject{Key: fmt.Sprintf("data/z%04d", 128-index), Size: 1, ETag: "\"first\"", LastModified: firstDate}
			}
			lastObjects := []checkpointHTTPObject{
				{Key: "data/a", Size: 2, ETag: "\"last\"", LastModified: "2026-10-29T00:00:00Z"},
				{Key: "data/m", Size: 2, ETag: "\"last\"", LastModified: "2026-10-29T00:00:00Z"},
				{Key: "data/b", Size: 2, ETag: "\"last\"", LastModified: "2026-10-29T00:00:00Z"},
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				query := r.URL.Query()
				token := query.Get("continuation-token")
				mu.Lock()
				requests = append(requests, token)
				mu.Unlock()
				if r.URL.Path != "/bucket" || r.Method != http.MethodGet || query.Get("list-type") != "2" || query.Get("prefix") != "data/" ||
					(token == "" && query.Get("start-after") != "data/0") || (token != "" && query.Get("start-after") != "") {
					t.Errorf("unexpected S3 request: %s %s", r.Method, r.URL)
					w.WriteHeader(400)
					return
				}
				var page checkpointHTTPPage
				switch token {
				case "":
					page = checkpointHTTPPage{IsTruncated: true, NextContinuationToken: "resume/after-first+=", Contents: firstObjects}
				case "resume/after-first+=":
					if gate.CompareAndSwap(true, false) {
						close(reached)
						<-r.Context().Done()
						return
					}
					if test.rejectResume {
						w.Header().Set("Content-Type", "application/xml")
						w.WriteHeader(400)
						fmt.Fprint(w, "<Error><Code>InvalidToken</Code><Message>Invalid saved listing token</Message></Error>")
						return
					}
					page = checkpointHTTPPage{IsTruncated: true, NextContinuationToken: "resume/end+=", Contents: lastObjects}
				case "resume/end+=":
					// Final empty pages still need SourceDone, otherwise reopening
					// a completed scan could confuse the empty cursor with start.
				default:
					t.Errorf("unexpected continuation token: %q", token)
					w.WriteHeader(400)
					return
				}
				w.Header().Set("Content-Type", "application/xml")
				if err := xml.NewEncoder(w).Encode(page); err != nil {
					t.Error(err)
				}
			}))
			t.Cleanup(server.Close)
			factory := checkpointHTTPFactory{server: server}
			path := filepath.Join(t.TempDir(), "agent.db")
			scanner, db := newScanner(t, path, factory, Options{})
			spec := Spec{ParentID: 1, Generation: 1, Source: execution.Endpoint{Kind: "S3", Ref: "saved", Bucket: "bucket"}, Prefix: "data/", StartKey: "data/0",
				ModifiedFrom: time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)}
			if _, err := scanner.Start(ctx, spec); err != nil {
				t.Fatal(err)
			}
			waitReached(t, reached)
			before := waitS3Checkpoint(t, scanner)
			firstCount := int64(129)
			if test.filteredFirst {
				firstCount = 0
			}
			if before.TotalObjects != firstCount || before.SeenObjects != firstCount || before.TotalBytes != firstCount || before.Checkpoint.SourceDone {
				t.Fatalf("page checkpoint did not cover its object batches: %+v", before)
			}
			assertBatches(t, scanner, 1, int(firstCount))
			if _, err := scanner.Stop(ctx, 1, 1); err != nil {
				t.Fatal(err)
			}
			if err := scanner.Close(); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			// New database handle, scanner, and S3 client: the cursor must come
			// from committed state, not any previous process's memory.
			scanner, _ = newScanner(t, path, factory, Options{})
			if _, err := scanner.Resume(ctx, 1, 2); err != nil {
				t.Fatal(err)
			}
			wantedStatus := Completed
			if test.rejectResume {
				wantedStatus = Failed
			}
			after := waitStatus(t, scanner, wantedStatus)
			if after.Checkpoint == nil || after.Checkpoint.Pass != before.Checkpoint.Pass {
				t.Fatalf("resume replaced its persistent pass: before=%+v after=%+v", before.Checkpoint, after.Checkpoint)
			}
			expectedRequests := []string{"", "resume/after-first+=", "resume/after-first+="}
			if test.rejectResume {
				if after.ScanComplete || after.TotalObjects != firstCount || after.SeenObjects != firstCount || after.LastBatch != before.LastBatch ||
					after.Checkpoint.Cursor != before.Checkpoint.Cursor || !strings.Contains(after.Error, "saved S3 listing cursor was rejected") {
					t.Fatalf("token failure lost the checkpoint or skipped data: %+v", after)
				}
				assertBatches(t, scanner, 2, int(firstCount))
			} else {
				expectedRequests = append(expectedRequests, "resume/end+=")
				if !after.ScanComplete || !after.Checkpoint.SourceDone || after.Checkpoint.Cursor != "" || after.TotalObjects != firstCount+3 ||
					after.SeenObjects != firstCount+3 || after.TotalBytes != firstCount+6 || after.MissingObjects != 0 || after.Warning != "" || after.Error != "" {
					t.Fatalf("resume lost or duplicated source statistics: %+v", after)
				}
				assertBatches(t, scanner, 2, int(firstCount+3))
			}
			mu.Lock()
			defer mu.Unlock()
			if !reflect.DeepEqual(requests, expectedRequests) {
				t.Fatalf("resume restarted source listing: got %v want %v", requests, expectedRequests)
			}
		})
	}
}
