package task

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/discovery"
)

func TestAssignmentCheckpointCommitsWithManifestAndRejectsScanAhead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.db")
	db, repo := openTest(t, path)
	p, _, err := repo.Create(testSpec(), "checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	batch := assignmentPage()[0]
	batch.Checkpoint = &discovery.RecoveryCheckpoint{Version: 1, Sequence: 1, ThroughBatch: 2, Objects: 2, Bytes: 2048, Cursor: "page-two"}
	if _, err := repo.AssignPage(p.ID, p.Generation, []Assignment{batch}); !errors.Is(err, ErrConflict) {
		t.Fatalf("accepted scanner cursor ahead of assigned work: %v", err)
	}
	current, err := repo.Get(p.ID)
	if err != nil || current.ChildCount != 0 || current.DispatchedThrough != 0 || current.ScanCheckpoint != nil {
		t.Fatalf("failed transaction partially committed: %+v %v", current, err)
	}
	batch.Checkpoint = &discovery.RecoveryCheckpoint{Version: 1, Sequence: 1, ThroughBatch: 1, Objects: 1, Bytes: 1024, Cursor: "page-one"}
	if _, err := repo.AssignPage(p.ID, p.Generation, []Assignment{batch}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, repo = openTest(t, path)
	defer db.Close()
	current, err = repo.Get(p.ID)
	if err != nil || current.ChildCount != 1 || current.ScanCheckpoint == nil || *current.ScanCheckpoint != *batch.Checkpoint {
		t.Fatalf("restart lost assigned page checkpoint: %+v %v", current, err)
	}
}

func TestScanProgressCheckpointKeepsFilteredPagesWithoutPrivateScanAhead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.db")
	db, repo := openTest(t, path)
	defer db.Close()
	p, _, err := repo.Create(testSpec(), "filtered-page-checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	empty := &discovery.RecoveryCheckpoint{Version: 1, Sequence: 1, Cursor: "filtered-first-page"}
	if err := repo.ScanProgressForScanner(p.ID, p.Generation, 0, 0, 0, 0, false, "", empty); err != nil {
		t.Fatal(err)
	}
	current, err := repo.Get(p.ID)
	if err != nil || current.ScanCheckpoint == nil || *current.ScanCheckpoint != *empty {
		t.Fatalf("filtered-only page checkpoint lost: %+v %v", current.ScanCheckpoint, err)
	}
	ahead := &discovery.RecoveryCheckpoint{Version: 1, Sequence: 2, ThroughBatch: 1, Objects: 1, Bytes: 1024, Cursor: "unassigned-next-page"}
	if err := repo.ScanProgressForScanner(p.ID, p.Generation, 0, 1, 1024, 1, false, "", ahead); err != nil {
		t.Fatal(err)
	}
	current, err = repo.Get(p.ID)
	if err != nil || *current.ScanCheckpoint != *empty {
		t.Fatalf("private scan-ahead cursor was accepted: %+v %v", current.ScanCheckpoint, err)
	}
	batch := assignmentPage()[0]
	batch.Checkpoint = ahead
	if _, err := repo.AssignPage(p.ID, p.Generation, []Assignment{batch}); err != nil {
		t.Fatal(err)
	}
	// A delayed retry of an older observation must not regress the boundary.
	if err := repo.ScanProgressForScanner(p.ID, p.Generation, 0, 1, 1024, 1, false, "", empty); err != nil {
		t.Fatal(err)
	}
	current, err = repo.Get(p.ID)
	if err != nil || *current.ScanCheckpoint != *ahead {
		t.Fatalf("delayed observation regressed checkpoint: %+v %v", current.ScanCheckpoint, err)
	}
}

func TestScannerCheckpointNewEpochRebindsCursorWithoutReusingOldOwner(t *testing.T) {
	old := &discovery.RecoveryCheckpoint{Version: 1, Sequence: 100, ThroughBatch: 1, Objects: 1, Bytes: 1024, Cursor: "old-mount", SourceRoot: "/old"}
	p := Parent{DispatchedThrough: 1, DispatchedObjects: 1, DispatchedBytes: 1024, ScanCheckpoint: old, Failover: &FailoverState{ScannerEpoch: 1}}
	next := *old
	next.Epoch, next.Cursor, next.SourceRoot = 1, "new-mount", "/new"
	if err := retainScanCheckpoint(&p, &next); err != nil {
		t.Fatal("new mount kept the same safe page boundary but was rejected", err)
	}
	if *p.ScanCheckpoint != next {
		t.Fatal(p.ScanCheckpoint)
	}
	if err := retainScanCheckpoint(&p, old); !errors.Is(err, ErrConflict) {
		t.Fatalf("old scanner checkpoint was admitted after takeover: %v", err)
	}
}

// The scan protocol a checkpoint requires is not part of its identity: an
// Agent or a Master that predates the field reports or saves the same page
// without it. The stricter requirement is kept, and dispatch goes on.
func TestScanCheckpointIgnoresALostProtocolRequirement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.db")
	db, repo := openTest(t, path)
	defer db.Close()
	p, _, err := repo.Create(testSpec(), "lost-protocol-requirement")
	if err != nil {
		t.Fatal(err)
	}
	page := assignmentPage()[:2]
	streamed := &discovery.RecoveryCheckpoint{Version: 1, Sequence: 1, ThroughBatch: 1, Objects: 1, Bytes: 1024, Cursor: "inside-a-streamed-directory", RequiresScanProtocol: 1}
	page[0].Checkpoint = streamed
	if _, err := repo.AssignPage(p.ID, p.Generation, page[:1]); err != nil {
		t.Fatal(err)
	}
	// The scanner was downgraded: its State lost the field, and the next
	// batch carries the same checkpoint without it.
	lost := *streamed
	lost.RequiresScanProtocol = 0
	if err := repo.ScanProgressForScanner(p.ID, p.Generation, 0, 2, 2048, 2, false, "", &lost); err != nil {
		t.Fatalf("progress with the checkpoint that lost its requirement: %v", err)
	}
	page[1].Checkpoint = &lost
	if _, err := repo.AssignPage(p.ID, p.Generation, page[1:2]); err != nil {
		t.Fatalf("assignment with the checkpoint that lost its requirement: %v", err)
	}
	current, err := repo.Get(p.ID)
	if err != nil || current.DispatchedThrough != 2 || current.ScanCheckpoint == nil || *current.ScanCheckpoint != *streamed {
		t.Fatalf("retained %+v after dispatching %d: %v", current.ScanCheckpoint, current.DispatchedThrough, err)
	}
	// A Master that dropped the field is upgraded again: the scanner's report
	// of the same page restores the requirement.
	retained := Parent{DispatchedThrough: 1, DispatchedObjects: 1, DispatchedBytes: 1024, ScanCheckpoint: &lost}
	if err := retainScanCheckpoint(&retained, streamed); err != nil || *retained.ScanCheckpoint != *streamed {
		t.Fatalf("requirement not restored: %+v %v", retained.ScanCheckpoint, err)
	}
	// Any other difference in the same page is still a conflict.
	moved := *streamed
	moved.Cursor = "elsewhere"
	if err := retainScanCheckpoint(&retained, &moved); !errors.Is(err, ErrConflict) {
		t.Fatalf("a different page under the same sequence was accepted: %v", err)
	}
}
