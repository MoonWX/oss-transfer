package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIRequestsRespectCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	called := make(chan struct{})
	client := &ApiClient{baseUrl: "http://cli-test.invalid/api/master", requestContext: ctx, client: &http.Client{Transport: adminTestTransport(func(r *http.Request) (*http.Response, error) {
		close(called)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}}
	done := make(chan error, 1)
	go func() { _, err := client.Get("/status"); done <- err }()
	<-called
	cancel()
	if err := <-done; err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("cancel error: %v", err)
	}
	path := filepath.Join(t.TempDir(), "report.zip")
	client.client.Transport = adminTestTransport(func(r *http.Request) (*http.Response, error) { return nil, r.Context().Err() })
	if err := client.Download("GET", "/task-reports/224", nil, path); err == nil {
		t.Fatal("cancelled download succeeded")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("cancelled report published")
	}
}
