package discovery

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func TestHigherGenerationStopFencesLiveScanAndSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	root := sourceFiles(t, 260)
	factory := &localFactory{gateAfter: 128, reached: make(chan struct{}), release: make(chan struct{}), allowExit: make(chan struct{})}
	path := filepath.Join(t.TempDir(), "agent.db")
	scanner, db := newScanner(t, path, factory, Options{})
	t.Cleanup(func() {
		select {
		case <-factory.allowExit:
		default:
			close(factory.allowExit)
		}
	})
	if _, err := scanner.Start(ctx, localSpec(root)); err != nil {
		t.Fatal(err)
	}
	waitReached(t, factory.reached)
	before, err := scanner.Status(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	current, err := scanner.Stop(ctx, 1, 3)
	if err != nil || current.Status != Stopped || current.Generation != 3 || current.Spec.Generation != 3 || current.TotalObjects != 128 {
		t.Fatalf("future Stop failed: %+v %v", current, err)
	}
	if err := scanner.commitBatch(ctx, before, []transferstore.Object{{Key: "late", Size: 12}}); !errors.Is(err, ErrGeneration) {
		t.Fatalf("old scan callback bypassed fence: %v", err)
	}
	if _, err := scanner.Stop(ctx, 1, 2); !errors.Is(err, ErrGeneration) {
		t.Fatalf("older Stop accepted: %v", err)
	}
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err := scanner.Resume(short, 1, 4); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("resume did not wait for canceled old scan: %v", err)
	}
	close(factory.allowExit)
	if err := scanner.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, _ := newScanner(t, path, &localFactory{}, Options{})
	spec := localSpec(root)
	spec.Generation = 3
	if _, err := reopened.Start(ctx, spec); !errors.Is(err, ErrStopped) {
		t.Fatalf("restart lost stop fence: %v", err)
	}
	if _, err := reopened.Resume(ctx, 1, 4); err != nil {
		t.Fatal(err)
	}
	completed := waitStatus(t, reopened, Completed)
	if completed.Generation != 4 || completed.TotalObjects != 260 || completed.LastBatch != 3 {
		t.Fatalf("resume lost/duplicated manifest: %+v", completed)
	}
}

func TestHigherGenerationStopPersistsCompletedAndEmptyScans(t *testing.T) {
	ctx := context.Background()
	scanner, _ := newScanner(t, filepath.Join(t.TempDir(), "agent.db"), &localFactory{}, Options{})
	root := sourceFiles(t, 1)
	if _, err := scanner.Start(ctx, localSpec(root)); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, scanner, Completed)
	if completed, err := scanner.Stop(ctx, 1, 9); err != nil || completed.Generation != 9 || !completed.ScanComplete || completed.Status != Completed {
		t.Fatalf("completed scan failed to advance fence: %+v %v", completed, err)
	}
	if _, err := scanner.Start(ctx, localSpec(root)); !errors.Is(err, ErrGeneration) {
		t.Fatalf("completed scan accepted old generation: %v", err)
	}
	if empty, err := scanner.Stop(ctx, 2, 8); err != nil || empty.Generation != 8 || empty.Status != Stopped {
		t.Fatalf("unknown scan did not get a durable fence: %+v %v", empty, err)
	}
	if _, err := scanner.Stop(ctx, 2, 7); !errors.Is(err, ErrGeneration) {
		t.Fatalf("empty fence accepted older stop: %v", err)
	}
}
