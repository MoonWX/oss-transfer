package comparison

// Shared test helpers. Add new helpers in your own test files instead of
// changing these, so parallel work on the package merges cleanly.

import (
	"bytes"
	"context"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

type inspectedStore struct {
	transferstore.Store
	opens  atomic.Int64
	stats  atomic.Int64
	mutate func()
	etag   string
}

// waitWorkers blocks until every started comparison has written its terminal
// state. The timeout is only a hang guard; correctness never depends on speed.
func waitWorkers(t *testing.T, m *Manager) {
	t.Helper()
	done := make(chan struct{})
	go func() { m.workers.Wait(); close(done) }()
	guard := 5 * time.Minute
	if d, ok := t.Deadline(); ok && time.Until(d)-10*time.Second < guard {
		guard = time.Until(d) - 10*time.Second
	}
	select {
	case <-done:
	case <-time.After(guard):
		t.Fatal("comparison worker did not exit")
	}
}

func (s *inspectedStore) Stat(ctx context.Context, key string) (transferstore.Object, error) {
	s.stats.Add(1)
	o, e := s.Store.Stat(ctx, key)
	if s.etag != "" {
		o.ETag = s.etag
	}
	return o, e
}

func (s *inspectedStore) List(ctx context.Context, o transferstore.ListOptions) (transferstore.Page, error) {
	p, e := s.Store.List(ctx, o)
	for i := range p.Objects {
		if s.etag != "" {
			p.Objects[i].ETag = s.etag
		}
	}
	return p, e
}

func (s *inspectedStore) Open(ctx context.Context, key string, o transferstore.OpenOptions) (io.ReadCloser, transferstore.Object, error) {
	s.opens.Add(1)
	r, a, e := s.Store.Open(ctx, key, o)
	if e == nil && s.mutate != nil {
		s.mutate()
	}
	return r, a, e
}

func local(t *testing.T) *inspectedStore {
	t.Helper()
	s, e := transferstore.NewLocal(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return &inspectedStore{Store: s}
}

func put(t *testing.T, s transferstore.Store, key, value string) {
	t.Helper()
	_, e := s.Write(context.Background(), key, bytes.NewBufferString(value), transferstore.WriteOptions{Size: int64(len(value)), Overwrite: true})
	if e != nil {
		t.Fatal(e)
	}
}

func request() Request {
	return Request{ID: "test", Source: execution.Endpoint{Kind: "S3", Ref: "src", Bucket: "src"}, Destination: execution.Endpoint{Kind: "S3", Ref: "dst", Bucket: "dst"}, Scope: "prefix", Mode: "metadata", SourcePrefix: "src/", DestinationPrefix: "backup/"}
}

func collect(t *testing.T, r Request, a, b transferstore.Store) []Row {
	t.Helper()
	var rows []Row
	err := Run(context.Background(), r, a, b, nil, RunOptions{}, func(row Row) error { rows = append(rows, row); return nil })
	if err != nil {
		t.Fatal(err)
	}
	return rows
}
