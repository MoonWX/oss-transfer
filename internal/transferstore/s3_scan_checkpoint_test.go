package transferstore

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func checkpointS3(t *testing.T, server *httptest.Server) *S3 {
	t.Helper()
	store, err := NewS3(S3Config{Endpoint: server.URL, Region: "test", Bucket: "bucket", AccessKey: "test-access", SecretKey: "test-secret", PathStyle: true, RetryMaxAttempts: 1, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestS3ScanPagesResumeFromCommittedToken(t *testing.T) {
	var mu sync.Mutex
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		token := query.Get("continuation-token")
		mu.Lock()
		requests = append(requests, token)
		mu.Unlock()
		if query.Get("list-type") != "2" || query.Get("prefix") != "p/" || query.Get("max-keys") != "2" ||
			(token == "" && query.Get("start-after") != "p/0") || (token != "" && query.Get("start-after") != "") {
			t.Errorf("wrong LIST scope: %s", r.URL.RawQuery)
			protocolError(w, 400, "InvalidArgument")
			return
		}
		type entry struct {
			Key  string
			Size int
		}
		type listing struct {
			XMLName               xml.Name `xml:"ListBucketResult"`
			IsTruncated           bool
			NextContinuationToken string `xml:",omitempty"`
			Contents              []entry
		}
		var page listing
		switch token {
		case "":
			// Deliberately unordered: a last-Key shortcut would miss p/m.
			page = listing{IsTruncated: true, NextContinuationToken: "opaque/one+=", Contents: []entry{{"p/z", 3}, {"p/a", 1}}}
		case "opaque/one+=":
			page = listing{IsTruncated: true, NextContinuationToken: "opaque/two+="}
		case "opaque/two+=":
			page = listing{IsTruncated: true, NextContinuationToken: "opaque/three+=", Contents: []entry{{"p/m", 2}}}
		case "opaque/three+=":
			// A final empty page still needs a durable completion checkpoint.
		default:
			t.Errorf("unexpected continuation token %q", token)
			protocolError(w, 400, "InvalidToken")
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		if err := xml.NewEncoder(w).Encode(page); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	options := ListOptions{Prefix: "p/", StartAfter: "p/0", Limit: 2}
	checkpoint := filepath.Join(t.TempDir(), "cursor")
	var keys []string
	commit := func(page Page) error {
		if err := os.WriteFile(checkpoint, []byte(page.NextCursor), 0600); err != nil {
			return err
		}
		for _, object := range page.Objects {
			keys = append(keys, object.Key)
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := checkpointS3(t, server).ScanPages(ctx, options, func(page Page) error {
		if err := commit(page); err != nil {
			return err
		}
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("scan did not stop after committed page: %v", err)
	}
	readCheckpoint := func() string {
		t.Helper()
		raw, err := os.ReadFile(checkpoint)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	options.Cursor = readCheckpoint()
	if options.Cursor == "" {
		t.Fatal("first page omitted its continuation token")
	}
	failedCommit := errors.New("checkpoint persistence failed")
	err = checkpointS3(t, server).ScanPages(context.Background(), options, func(page Page) error {
		if len(page.Objects) != 0 || page.NextCursor == "" {
			t.Fatalf("expected an empty advancing page: %+v", page)
		}
		return failedCommit
	})
	if !errors.Is(err, failedCommit) || readCheckpoint() != options.Cursor {
		t.Fatalf("failed page advanced its checkpoint: %v", err)
	}
	pages := 0
	err = checkpointS3(t, server).ScanPages(context.Background(), options, func(page Page) error {
		pages++
		return commit(page)
	})
	if err != nil || pages != 3 || readCheckpoint() != "" || !reflect.DeepEqual(keys, []string{"p/z", "p/a", "p/m"}) {
		t.Fatalf("resume missed or repeated objects: pages=%d keys=%v err=%v", pages, keys, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(requests, []string{"", "opaque/one+=", "opaque/one+=", "opaque/two+=", "opaque/three+="}) {
		t.Fatalf("resume replayed committed pages or advanced a rejected page: %v", requests)
	}
}

func TestS3ScanPagesRejectsInvalidCursorWithoutRestarting(t *testing.T) {
	for _, test := range []struct {
		code, message string
		invalidCursor bool
	}{
		{"InvalidToken", "Token is invalid", true},
		{"InvalidArgument", "The continuation token provided is incorrect", true},
		{"AccessDenied", "Access denied", false},
		{"InvalidArgument", "Invalid prefix", false},
	} {
		t.Run(test.code+"/"+test.message, func(t *testing.T) {
			var mu sync.Mutex
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				requests++
				mu.Unlock()
				if r.URL.Query().Get("continuation-token") != "expired-token" {
					t.Errorf("invalid cursor restarted listing: %s", r.URL.RawQuery)
				}
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(400)
				fmt.Fprintf(w, "<Error><Code>%s</Code><Message>%s</Message></Error>", test.code, test.message)
			}))
			defer server.Close()
			store := checkpointS3(t, server)
			options := ListOptions{Prefix: "p/", Cursor: makeCursor("s3", store.scope, "p/", "", "expired-token")}
			err := store.ScanPages(context.Background(), options, func(Page) error {
				t.Fatal("rejected cursor delivered a page")
				return nil
			})
			if err == nil || errors.Is(err, ErrInvalidCursor) != test.invalidCursor {
				t.Fatalf("incorrect rejection classification: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if requests != 1 {
				t.Fatalf("cursor rejection made %d requests", requests)
			}
		})
	}
}

func TestS3ScanPagesCursorScopeAndCancellationBeforeRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("invalid scan issued a network request")
		w.WriteHeader(500)
	}))
	defer server.Close()
	store := checkpointS3(t, server)
	for _, cursor := range []string{"not-a-cursor", makeCursor("s3", "another-store", "p/", "", "next"), makeCursor("s3", store.scope, "other/", "", "next"), makeCursor("s3", store.scope, "p/", "p/start", "next")} {
		err := store.ScanPages(context.Background(), ListOptions{Prefix: "p/", Cursor: cursor}, func(Page) error { return nil })
		if !errors.Is(err, ErrInvalidCursor) {
			t.Fatalf("cursor scope was accepted: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.ScanPages(ctx, ListOptions{}, func(Page) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled scan continued: %v", err)
	}
}
