package ratelimit

import (
	"bytes"
	"context"
	"github.com/MoonWX/oss-transfer/internal/state"
	"io"
	"path/filepath"
	"testing"
	"time"
)

func TestMaintenanceReaderUsesSharedNodeBudgetWithoutTaskAllocation(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	l, err := New(db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ctx := context.Background()
	if err = l.ApplyNode(ctx, NodeLimit{Revision: 1, BandwidthBytesPerSecond: 20 << 10}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	reader, err := l.WrapNode(ctx, io.NopCloser(bytes.NewReader(make([]byte, 4<<10))), 4<<10)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err = io.Copy(io.Discard, reader); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 170*time.Millisecond {
		t.Fatal("maintenance read bypassed node bandwidth")
	}
	if len(l.parents) != 0 {
		t.Fatal("maintenance created a task allocation")
	}
}
