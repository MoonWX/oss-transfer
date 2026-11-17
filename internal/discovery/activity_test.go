package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// A running attempt reports what its source scan is doing beside its State,
// never in it: the activity is not persisted, belongs to that attempt alone
// and ends with it.
func TestScanActivityBelongsToTheRunningAttemptOnly(t *testing.T) {
	root := checkpointSource(t)
	factory := &checkpointLocalFactory{reached: make(chan struct{})}
	scanner, db := newScanner(t, filepath.Join(t.TempDir(), "agent.db"), factory, Options{})
	ctx := context.Background()
	started, err := scanner.Start(ctx, localSpec(root))
	if err != nil {
		t.Fatal(err)
	}
	waitReached(t, factory.reached)
	// The first page was a-done and its 128 files, read from the two root
	// names and a-done's 128.
	activity := scanner.Activity(1, started.Attempt)
	if activity == nil || activity.Mode != transferstore.ScanModeSorted || activity.NamesRead != 130 || activity.Examined != 129 ||
		activity.AlreadyFound != 0 || activity.Vanished != 0 {
		t.Fatalf("activity of the running attempt: %+v", activity)
	}
	if scanner.Activity(1, "another-attempt") != nil || scanner.Activity(2, started.Attempt) != nil || scanner.Activity(1, "") != nil {
		t.Fatal("activity reported for an attempt that is not running")
	}
	var raw json.RawMessage
	if err := db.View(func(tx *state.Tx) error {
		_, err := tx.Get(states, parentKey(1), &raw)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"activity", "namesRead", "examined"} {
		if bytes.Contains(raw, []byte(field)) {
			t.Fatalf("persisted state carries %s: %s", field, raw)
		}
	}
	if _, err := scanner.Stop(ctx, 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := scanner.QuiesceParent(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if scanner.Activity(1, started.Attempt) != nil {
		t.Fatal("activity outlived its attempt")
	}
}
