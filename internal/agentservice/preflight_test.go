package agentservice

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func TestPreflightCleanupFencesLateWritesAndRecoversAfterRestart(t *testing.T) {
	ctx := context.Background()
	database, root := filepath.Join(t.TempDir(), "agent.db"), t.TempDir()
	db, err := state.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	p := &preflightProbes{store: db}
	marker, err := transferstore.NewSharedProbe()
	if err != nil {
		t.Fatal(err)
	}
	request := PreflightProbeRequest{Root: root, Action: "write", Probe: marker}
	if err := p.execute(ctx, request); err != nil {
		t.Fatal(err)
	}
	request.Action = "remove"
	if err := p.execute(ctx, request); err != nil {
		t.Fatal(err)
	}
	request.Action = "write"
	if err := p.execute(ctx, request); err == nil {
		t.Fatal("late write recreated a cleaned probe")
	}
	marker, _ = transferstore.NewSharedProbe()
	request.Probe = marker
	if err := p.execute(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = state.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p = &preflightProbes{store: db}
	if err := p.fencePending(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, marker.Key)); err != nil {
		t.Fatal("startup fence unexpectedly touched the storage directory")
	}
	if err := p.sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, marker.Key)); !os.IsNotExist(err) {
		t.Fatal("restart did not clean the persisted probe")
	}
	if err := p.execute(ctx, request); err == nil {
		t.Fatal("restart lost the cleanup fence")
	}
	marker, _ = transferstore.NewSharedProbe()
	request.Probe = marker
	if err := p.execute(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, marker.Key), []byte("replaced user data"), 0600); err != nil {
		t.Fatal(err)
	}
	request.Action = "remove"
	if err := p.execute(ctx, request); !errors.Is(err, transferstore.ErrSharedProbe) {
		t.Fatal("cleanup did not reject a changed file", err)
	}
	if err := p.sweep(ctx); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, marker.Key))
	if err != nil || string(data) != "replaced user data" {
		t.Fatal("recovery removed an unrelated replacement file")
	}
}

func TestPreflightRetainsCleanupUntilOriginalDirectoryReturns(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	root, detached := filepath.Join(base, "mount"), filepath.Join(base, "detached")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p := &preflightProbes{store: db}
	probe, err := transferstore.NewSharedProbe()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.execute(ctx, PreflightProbeRequest{Root: root, Action: "write", Probe: probe}); err != nil {
		t.Fatal(err)
	}
	// Simulate an unmounted filesystem exposing an empty underlying directory.
	if err := os.Rename(root, detached); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := p.fencePending(); err != nil {
		t.Fatal(err)
	}
	if err := p.sweep(ctx); err != nil {
		t.Fatal(err)
	}
	var record preflightProbeRecord
	if err := db.View(func(tx *state.Tx) error { _, err := tx.Get(preflightProbeBucket, probe.Key, &record); return err }); err != nil {
		t.Fatal(err)
	}
	if !record.Closed || record.Cleaned {
		t.Fatal("temporary unmount was mistaken for a completed cleanup")
	}
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(detached, root); err != nil {
		t.Fatal(err)
	}
	if err := p.sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, probe.Key)); !os.IsNotExist(err) {
		t.Fatal("restored mount still has the tracked probe")
	}
	if err := db.View(func(tx *state.Tx) error { _, err := tx.Get(preflightProbeBucket, probe.Key, &record); return err }); err != nil || !record.Cleaned {
		t.Fatal("restored mount cleanup was not persisted", err)
	}
}

func TestStalledPreflightDoesNotBlockDifferentProbe(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p := &preflightProbes{store: db}
	stalled, _ := transferstore.NewSharedProbe()
	release, err := p.lock(context.Background(), stalled.Key)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	root := t.TempDir()
	if err := p.execute(ctx, PreflightProbeRequest{Root: root, Action: "remove", Probe: stalled}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("waiting for a stalled probe ignored cancellation", err)
	}
	other, _ := transferstore.NewSharedProbe()
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.execute(ctx, PreflightProbeRequest{Root: root, Action: "write", Probe: other}); err != nil {
		t.Fatal("unrelated probe was blocked", err)
	}
	if err := p.execute(ctx, PreflightProbeRequest{Root: root, Action: "remove", Probe: other}); err != nil {
		t.Fatal(err)
	}
}
