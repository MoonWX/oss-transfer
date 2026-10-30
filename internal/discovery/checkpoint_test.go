package discovery

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// Unlike observedLocal, this wrapper deliberately exposes ScanPages. These
// checks must exercise the production checkpoint path, never the Scan fallback.
type checkpointLocalFactory struct {
	mu      sync.Mutex
	cursors []string
	keys    []string
	reached chan struct{}
}

type checkpointLocalStore struct {
	*transferstore.Local
	factory *checkpointLocalFactory
}

func (f *checkpointLocalFactory) OpenEndpoint(ctx context.Context, endpoint execution.Endpoint) (transferstore.Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	store, err := transferstore.NewLocal(endpoint.Root)
	if err != nil {
		return nil, err
	}
	return &checkpointLocalStore{Local: store, factory: f}, nil
}

func (s *checkpointLocalStore) Scan(context.Context, transferstore.ListOptions, func(transferstore.Object) error) error {
	return errors.New("checkpoint test unexpectedly used the full-scan fallback")
}

func (s *checkpointLocalStore) ScanPages(ctx context.Context, options transferstore.ListOptions, emit func(transferstore.Page) error) error {
	f := s.factory
	f.mu.Lock()
	f.cursors = append(f.cursors, options.Cursor)
	firstRun := len(f.cursors) == 1
	f.mu.Unlock()
	// One directory plus its 128 files fills the first page exactly, leaving
	// the next directory outside the committed traversal stack.
	options.Limit = 129
	pages := 0
	return s.Local.ScanPages(ctx, options, func(page transferstore.Page) error {
		f.mu.Lock()
		for _, object := range page.Objects {
			f.keys = append(f.keys, object.Key)
		}
		f.mu.Unlock()
		if err := emit(page); err != nil {
			return err
		}
		pages++
		if firstRun && pages == 1 && f.reached != nil {
			close(f.reached)
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	})
}

func (f *checkpointLocalFactory) observed() ([]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cursors...), append([]string(nil), f.keys...)
}

func checkpointSource(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, part := range []struct {
		name  string
		count int
	}{{"a-done", 128}, {"z-rest", 132}} {
		directory := filepath.Join(root, part.name)
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
		for index := 0; index < part.count; index++ {
			if err := os.WriteFile(filepath.Join(directory, fmt.Sprintf("%04d.bin", index)), []byte("test data"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

func waitSourceCheckpoint(t *testing.T, scanner *Scanner) State {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, err := scanner.Status(context.Background(), 1)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status == Failed {
			t.Fatal("checkpoint scan failed:", current.Error)
		}
		if current.Checkpoint != nil && current.Checkpoint.Cursor != "" {
			if current.Checkpoint.Version != 1 || current.Checkpoint.Pass == "" || current.Checkpoint.SourceDone ||
				current.TotalObjects != 128 || current.SeenObjects != 128 || current.LastBatch != 1 {
				t.Fatalf("cursor was not committed with the whole first page: %+v", current)
			}
			return current
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("source page cursor was not persisted while its callback was blocked")
	return State{}
}

func assertCheckpointContinuation(t *testing.T, f *checkpointLocalFactory, expectedCursor string) {
	t.Helper()
	cursors, keys := f.observed()
	if len(cursors) != 1 || cursors[0] == "" || cursors[0] != expectedCursor {
		t.Fatalf("source reopened without its committed cursor: %q", cursors)
	}
	if len(keys) != 132 {
		t.Fatalf("resume inspected %d objects instead of the remaining 132", len(keys))
	}
	for _, key := range keys {
		if !strings.HasPrefix(key, "z-rest/") {
			t.Fatalf("resume revisited a completed subtree: %s", key)
		}
	}
}

func TestCheckpointLocalStopReopenAndReleasedBatchResume(t *testing.T) {
	root := checkpointSource(t)
	path := filepath.Join(t.TempDir(), "agent.db")
	factory := &checkpointLocalFactory{reached: make(chan struct{})}
	scanner, db := newScanner(t, path, factory, Options{})
	if err := db.ClaimRole("agent"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := scanner.Start(ctx, localSpec(root)); err != nil {
		t.Fatal(err)
	}
	waitReached(t, factory.reached)
	before := waitSourceCheckpoint(t, scanner)
	if _, err := scanner.Ack(ctx, 1, 1, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Release(ctx, 1, 1, 1); err != nil {
		t.Fatal(err)
	}
	waitBatchesReleased(t, scanner, 1, 1)
	if _, err := scanner.Stop(ctx, 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := scanner.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// Mutation behind the saved cursor is deliberately not a new scan. A
	// cursorless replay would encounter this changed identity and fail.
	if err := os.WriteFile(filepath.Join(root, "a-done", "0000.bin"), []byte("already-scanned source has changed"), 0600); err != nil {
		t.Fatal(err)
	}
	resumedFactory := &checkpointLocalFactory{}
	resumed, _ := newScanner(t, path, resumedFactory, Options{})
	if _, err := resumed.Resume(ctx, 1, 2); err != nil {
		t.Fatal(err)
	}
	done := waitStatus(t, resumed, Completed)
	if done.TotalObjects != 260 || done.SeenObjects != 260 || done.TotalBytes != 260*9 || done.MissingObjects != 0 || done.Warning != "" ||
		done.LastBatch != 3 || done.Checkpoint == nil || done.Checkpoint.Pass != before.Checkpoint.Pass {
		t.Fatalf("checkpoint resume lost or duplicated discovery counters: %+v", done)
	}
	assertCheckpointContinuation(t, resumedFactory, before.Checkpoint.Cursor)
	assertBatchesFrom(t, resumed, 2, 1, 132)
}

func TestCheckpointReplaysPageAfterBatchCommitBeforeCursorCommit(t *testing.T) {
	root := checkpointSource(t)
	path := filepath.Join(t.TempDir(), "agent.db")
	ctx := context.Background()
	source, err := transferstore.NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	var listed []transferstore.Object
	if err := source.ScanPages(ctx, transferstore.ListOptions{Limit: 1000}, func(page transferstore.Page) error {
		listed = append(listed, page.Objects...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 260 {
		t.Fatalf("real source returned %d objects", len(listed))
	}
	scanner, db := newScanner(t, path, &checkpointLocalFactory{}, Options{})
	attempt := State{Spec: localSpec(root), Generation: 1, Status: Scanning, Attempt: "interrupted-run",
		Checkpoint: &Checkpoint{Version: 1, Pass: "stable-scan-pass"}}
	if err := db.Update(func(tx *state.Tx) error { return tx.Put(states, parentKey(1), attempt) }); err != nil {
		t.Fatal(err)
	}
	// An output batch became durable while its source page was still being
	// processed. The page marker has not arrived, so its cursor stays empty.
	if err := scanner.commitBatches(ctx, attempt, []pendingBatch{{objects: listed[:128]}}); err != nil {
		t.Fatal(err)
	}
	if err := scanner.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	factory := &checkpointLocalFactory{}
	resumed, _ := newScanner(t, path, factory, Options{})
	before, err := resumed.Status(ctx, 1)
	if err != nil || before.Status != Recoverable || before.TotalObjects != 128 || before.SeenObjects != 128 || before.LastBatch != 1 ||
		before.Checkpoint == nil || before.Checkpoint.Cursor != "" || before.Checkpoint.Pass != attempt.Checkpoint.Pass {
		t.Fatalf("batch-before-cursor restart lost durable scan state: %+v %v", before, err)
	}
	if _, err := resumed.Start(ctx, localSpec(root)); err != nil {
		t.Fatal(err)
	}
	done := waitStatus(t, resumed, Completed)
	if done.TotalObjects != 260 || done.SeenObjects != 260 || done.TotalBytes != 260*9 || done.MissingObjects != 0 || done.Warning != "" ||
		done.LastBatch != 3 || done.Checkpoint == nil || done.Checkpoint.Pass != attempt.Checkpoint.Pass || done.Attempt == attempt.Attempt {
		t.Fatalf("replayed source page duplicated work or lost scan identity: %+v", done)
	}
	cursors, keys := factory.observed()
	if len(cursors) != 1 || cursors[0] != "" || len(keys) != 260 {
		t.Fatalf("test did not replay the uncheckpointed page: cursors=%q objects=%d", cursors, len(keys))
	}
	assertBatches(t, resumed, 1, 260)
}

func TestCheckpointCrashProcess(t *testing.T) {
	if os.Getenv("OSS_DISCOVERY_CHECKPOINT_HELPER") != "1" {
		return
	}
	db, err := state.Open(os.Getenv("OSS_DISCOVERY_CHECKPOINT_DB"))
	if err != nil {
		t.Fatal(err)
	}
	factory := &checkpointLocalFactory{reached: make(chan struct{})}
	scanner, err := New(db, factory, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Start(context.Background(), localSpec(os.Getenv("OSS_DISCOVERY_CHECKPOINT_SOURCE"))); err != nil {
		t.Fatal(err)
	}
	waitReached(t, factory.reached)
	waitSourceCheckpoint(t, scanner)
	fmt.Println("source-checkpoint-committed")
	// Keep a timer alive until the parent kills the process, without graceful
	// shutdown or Close. A bare select could trigger the runtime deadlock check.
	time.Sleep(30 * time.Second)
	t.Fatal("parent did not kill the checkpoint process")
}

func TestCheckpointSurvivesActualProcessKill(t *testing.T) {
	root := checkpointSource(t)
	path := filepath.Join(t.TempDir(), "agent.db")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^TestCheckpointCrashProcess$", "-test.count=1")
	command.Env = append(os.Environ(), "OSS_DISCOVERY_CHECKPOINT_HELPER=1", "OSS_DISCOVERY_CHECKPOINT_DB="+path, "OSS_DISCOVERY_CHECKPOINT_SOURCE="+root)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer command.Process.Kill()
	lines := bufio.NewScanner(stdout)
	if !lines.Scan() || lines.Text() != "source-checkpoint-committed" {
		_ = command.Wait()
		t.Fatal("child did not reach a committed source cursor:", stderr.String())
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("checkpoint process was not killed")
	}
	factory := &checkpointLocalFactory{}
	scanner, _ := newScanner(t, path, factory, Options{})
	before, err := scanner.Status(context.Background(), 1)
	if err != nil || before.Status != Recoverable || before.Checkpoint == nil || before.Checkpoint.Cursor == "" {
		t.Fatalf("crash lost its source checkpoint: %+v %v", before, err)
	}
	if _, err := scanner.Start(context.Background(), localSpec(root)); err != nil {
		t.Fatal(err)
	}
	done := waitStatus(t, scanner, Completed)
	if done.TotalObjects != 260 || done.SeenObjects != 260 || done.MissingObjects != 0 || done.LastBatch != 3 || done.Checkpoint.Pass != before.Checkpoint.Pass {
		t.Fatalf("process restart duplicated work or treated skipped prefix as missing: %+v", done)
	}
	assertCheckpointContinuation(t, factory, before.Checkpoint.Cursor)
	assertBatches(t, scanner, 1, 260)
}

type unopenedCheckpointFactory struct{ opened bool }

func (f *unopenedCheckpointFactory) OpenEndpoint(context.Context, execution.Endpoint) (transferstore.Store, error) {
	f.opened = true
	return nil, errors.New("completed source must not be reopened")
}

func TestCheckpointSourceDoneFinishesAfterRestartWithoutReopeningSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	db, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	spec := localSpec(t.TempDir())
	// This is the crash window after the final source page committed but
	// before finish marked the entire scan complete and queued index cleanup.
	checkpoint := &Checkpoint{Version: 1, Pass: "completed-source-pass", SourceDone: true}
	current := State{Spec: spec, Generation: 1, Status: Scanning, Attempt: "interrupted-run",
		Checkpoint: checkpoint, TotalObjects: 1, TotalBytes: 9, SeenObjects: 1, LastBatch: 1}
	object := transferstore.Object{Key: "finished.bin", Size: 9}
	if err := db.Update(func(tx *state.Tx) error {
		if err := tx.Put(states, parentKey(1), current); err != nil {
			return err
		}
		if err := tx.Put(objects, objectKey(1, object.Key), discoveredObject{Object: object, SeenAttempt: checkpoint.Pass}); err != nil {
			return err
		}
		return tx.Put(batches, batchKey(1, 1), Batch{ParentID: 1, Generation: 1, Number: 1, Objects: []transferstore.Object{object}, Bytes: 9})
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	factory := &unopenedCheckpointFactory{}
	scanner, _ := newScanner(t, path, factory, Options{})
	if _, err := scanner.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	done := waitStatus(t, scanner, Completed)
	if factory.opened || done.TotalObjects != 1 || done.SeenObjects != 1 || done.MissingObjects != 0 || done.LastBatch != 1 {
		t.Fatalf("final-page recovery reopened source or reset counters: %+v (opened %t)", done, factory.opened)
	}
	assertBatches(t, scanner, 1, 1)
}

func TestCheckpointTakeoverDropsUnassignedPrivateCursor(t *testing.T) {
	root := checkpointSource(t)
	factory := &checkpointLocalFactory{}
	scanner, db := newScanner(t, filepath.Join(t.TempDir(), "agent.db"), factory, Options{})
	spec := localSpec(root)
	private := transferstore.Object{Key: "a-done/0000.bin", Size: 9}
	current := State{Spec: spec, Generation: 1, Status: Stopped, Attempt: "old-scanner",
		Checkpoint:   &Checkpoint{Version: 1, Pass: "old-pass", Cursor: "private-scan-ahead"},
		TotalObjects: 1, TotalBytes: 9, SeenObjects: 1, LastBatch: 1}
	if err := db.Update(func(tx *state.Tx) error {
		if err := tx.Put(states, parentKey(1), current); err != nil {
			return err
		}
		if err := tx.Put(objects, objectKey(1, private.Key), discoveredObject{Object: private, SeenAttempt: "old-pass"}); err != nil {
			return err
		}
		return tx.Put(batches, batchKey(1, 1), Batch{ParentID: 1, Generation: 1, Number: 1, Objects: []transferstore.Object{private}, Bytes: 9})
	}); err != nil {
		t.Fatal(err)
	}
	// Master has not assigned that private batch. A new scanner epoch must
	// rebuild from Master's manifest, not carry a cursor past unassigned work.
	seeded := seedTakeover(t, scanner, TakeoverRequest{Spec: spec, Epoch: 1})
	if seeded.Checkpoint != nil || seeded.LastBatch != 0 || seeded.TotalObjects != 0 {
		t.Fatalf("takeover retained the old scanner's private checkpoint: %+v", seeded)
	}
	if _, err := scanner.FinalizeTakeover(context.Background(), 1, 1, 1); err != nil {
		t.Fatal(err)
	}
	done := waitStatus(t, scanner, Completed)
	if done.TotalObjects != 260 || done.SeenObjects != 260 || done.MissingObjects != 0 || done.Checkpoint.Pass == "old-pass" {
		t.Fatalf("takeover skipped unassigned source objects: %+v", done)
	}
	cursors, keys := factory.observed()
	if len(cursors) != 1 || cursors[0] != "" || len(keys) != 260 {
		t.Fatalf("takeover started after private scan-ahead: cursors=%q objects=%d", cursors, len(keys))
	}
}

func TestCheckpointCannotAdvancePastRolledBackBatchesOrOldAttempt(t *testing.T) {
	scanner, db, attempt := commitFixture(t)
	ctx := context.Background()
	if err := scanner.beginCheckpoint(ctx, &attempt); err != nil {
		t.Fatal(err)
	}
	committed := Checkpoint{Version: 1, Pass: attempt.Checkpoint.Pass, Cursor: "committed-page"}
	if err := scanner.commitBatches(ctx, attempt, []pendingBatch{scanTestBatch(0), {checkpoint: &committed}}); err != nil {
		t.Fatal(err)
	}
	next := committed
	next.Cursor = "must-not-be-visible"
	conflict := scanTestBatch(0)
	conflict.objects[0].Size++
	// The marker is intentionally before the conflicting row. A transaction
	// failure after updating the cursor must roll back cursor and output alike.
	if err := scanner.commitBatches(ctx, attempt, []pendingBatch{scanTestBatch(1), {checkpoint: &next}, conflict}); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("conflicting page did not fail its transaction: %v", err)
	}
	for _, field := range []string{"generation", "epoch", "attempt"} {
		t.Run(field, func(t *testing.T) {
			stale := attempt
			switch field {
			case "generation":
				stale.Generation++
			case "epoch":
				stale.Epoch++
			case "attempt":
				stale.Attempt = "old-run"
			}
			if err := scanner.commitBatches(ctx, stale, []pendingBatch{scanTestBatch(1), {checkpoint: &next}}); !errors.Is(err, ErrGeneration) {
				t.Fatalf("stale %s advanced a checkpoint: %v", field, err)
			}
		})
	}
	current, err := scanner.Status(ctx, 1)
	if err != nil || current.Checkpoint.Cursor != committed.Cursor || current.TotalObjects != 128 || current.SeenObjects != 128 || current.LastBatch != 1 {
		t.Fatalf("failed page exposed a checkpoint or partial counters: %+v %v", current, err)
	}
	if err := db.View(func(tx *state.Tx) error {
		var object discoveredObject
		found, err := tx.Get(objects, objectKey(1, scanTestBatch(1).objects[0].Key), &object)
		if found {
			t.Error("rolled-back page left an object in the discovery index")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, found, err := scanner.Batch(ctx, 1, 1, 1); err != nil || found {
		t.Fatalf("rolled-back page left a public batch: %v", err)
	}
}
