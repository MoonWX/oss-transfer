package discovery

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

type localFactory struct {
	opens, scans, stats atomic.Int32
	gateAfter           int
	reached             chan struct{}
	release             chan struct{}
	allowExit           chan struct{}
}

type observedLocal struct {
	transferstore.Store
	factory *localFactory
	gate    bool
}

func (f *localFactory) OpenEndpoint(ctx context.Context, endpoint execution.Endpoint) (transferstore.Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	store, err := transferstore.NewLocal(endpoint.Root)
	if err != nil {
		return nil, err
	}
	return &observedLocal{Store: store, factory: f, gate: f.opens.Add(1) == 1 && f.reached != nil}, nil
}

func (s *observedLocal) Scan(ctx context.Context, options transferstore.ListOptions, emit func(transferstore.Object) error) error {
	s.factory.scans.Add(1)
	seen := 0
	return s.Store.Scan(ctx, options, func(object transferstore.Object) error {
		if err := emit(object); err != nil {
			return err
		}
		seen++
		if s.gate && seen == s.factory.gateAfter {
			close(s.factory.reached)
			select {
			case <-ctx.Done():
				if s.factory.allowExit != nil {
					<-s.factory.allowExit
				}
				return ctx.Err()
			case <-s.factory.release:
			}
		}
		return nil
	})
}

func (s *observedLocal) Stat(ctx context.Context, key string) (transferstore.Object, error) {
	s.factory.stats.Add(1)
	return s.Store.Stat(ctx, key)
}

func sourceFiles(t *testing.T, count int) string {
	t.Helper()
	root := t.TempDir()
	for index := 0; index < count; index++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("%04d.txt", index)), []byte("test data"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func newScanner(t *testing.T, path string, factory EndpointFactory, options Options) (*Scanner, *state.Store) {
	t.Helper()
	db, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	scanner, err := New(db, factory, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scanner.Close(); _ = db.Close() })
	return scanner, db
}

func localSpec(root string) Spec {
	return Spec{ParentID: 1, Generation: 1, Source: execution.Endpoint{Kind: "LOCAL", Root: root}}
}

func waitReached(t *testing.T, reached <-chan struct{}) {
	t.Helper()
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("real source scan did not reach the test gate")
	}
}

func waitStatus(t *testing.T, scanner *Scanner, wanted Status) State {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, err := scanner.Status(context.Background(), 1)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status == wanted {
			return current
		}
		if current.Status == Failed && wanted != Failed {
			t.Fatal("scan failed unexpectedly:", current.Error)
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("scan status did not settle")
	return State{}
}

func assertBatches(t *testing.T, scanner *Scanner, generation uint64, expected int) {
	t.Helper()
	assertBatchesFrom(t, scanner, generation, 0, expected)
}

func assertBatchesFrom(t *testing.T, scanner *Scanner, generation uint64, after int64, expected int) {
	t.Helper()
	seen := map[string]bool{}
	for {
		batch, found, err := scanner.Batch(context.Background(), 1, generation, after)
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			break
		}
		if batch.Number != after+1 || batch.Generation != generation || len(batch.Objects) < 1 || len(batch.Objects) > 128 || (len(batch.Objects) > 1 && batch.Bytes > 256<<20) {
			t.Fatal("batch numbering or bounds are incorrect")
		}
		var bytes int64
		for _, object := range batch.Objects {
			if seen[object.Key] {
				t.Fatal("source object was duplicated across batches")
			}
			seen[object.Key] = true
			bytes += object.Size
		}
		if batch.Bytes != bytes {
			t.Fatal("batch byte count is inconsistent")
		}
		after = batch.Number
	}
	if len(seen) != expected {
		t.Fatalf("got %d unique objects, want %d", len(seen), expected)
	}
}

func TestRealLocalScanStopResumeAfterReleasingAcknowledgedBatches(t *testing.T) {
	root := sourceFiles(t, 260)
	factory := &localFactory{gateAfter: 128, reached: make(chan struct{}), release: make(chan struct{}), allowExit: make(chan struct{})}
	scanner, db := newScanner(t, filepath.Join(t.TempDir(), "agent.db"), factory, Options{})
	if err := db.ClaimRole("agent"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		select {
		case <-factory.allowExit:
		default:
			close(factory.allowExit)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	first, err := scanner.Start(ctx, localSpec(root))
	if err != nil || first.Status != Scanning {
		t.Fatal("start failed", err)
	}
	cancel() // The API request finishing must not cancel the background scan.
	waitReached(t, factory.reached)
	if _, err := scanner.Start(context.Background(), localSpec(root)); err != nil || factory.opens.Load() != 1 {
		t.Fatal("duplicate start opened another scan", err)
	}
	current, err := scanner.Status(context.Background(), 1)
	if err != nil || current.TotalObjects != 128 || current.LastBatch != 1 || current.ScanComplete {
		t.Fatal("scan counters did not commit with its first batch", err)
	}
	if _, found, err := scanner.Batch(context.Background(), 1, 1, 1); err != nil || found {
		t.Fatal("an empty outgoing queue was mistaken for completion", err)
	}
	if _, err := scanner.Ack(context.Background(), 1, 1, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Release(context.Background(), 1, 1, 1); err != nil {
		t.Fatal(err)
	}
	waitBatchesReleased(t, scanner, 1, 1)
	if n := countCleanupRows(t, db, objects, 1); n != 128 {
		t.Fatalf("active scan lost its recovery index: %d", n)
	}
	if _, _, err := scanner.Batch(context.Background(), 1, 1, 0); !errors.Is(err, ErrBatchReleased) {
		t.Fatalf("released batch read: %v", err)
	}
	if _, err := scanner.Stop(context.Background(), 1, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Start(context.Background(), localSpec(root)); !errors.Is(err, ErrStopped) {
		t.Fatal("late Start bypassed stop fence", err)
	}
	short, timeout := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer timeout()
	if _, err := scanner.Resume(short, 1, 2); !errors.Is(err, context.DeadlineExceeded) || factory.opens.Load() != 1 {
		t.Fatal("resume did not wait for the old source handle to exit", err)
	}
	close(factory.allowExit)
	if _, err := scanner.Resume(context.Background(), 1, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Resume(context.Background(), 1, 2); err != nil {
		t.Fatal("retry after a lost Resume response was not idempotent", err)
	}
	if _, err := scanner.Stop(context.Background(), 1, 1); !errors.Is(err, ErrGeneration) {
		t.Fatal("old Stop stopped a new scan generation", err)
	}
	completed := waitStatus(t, scanner, Completed)
	if !completed.ScanComplete || completed.TotalObjects != 260 || completed.TotalBytes != 260*9 || completed.AckedThrough != 1 || completed.LastBatch != 3 {
		t.Fatal("resume duplicated or lost scan accounting")
	}
	if _, _, err := scanner.Batch(context.Background(), 1, 1, 0); !errors.Is(err, ErrGeneration) {
		t.Fatal("old-generation batch read was accepted", err)
	}
	if _, err := scanner.Ack(context.Background(), 1, 1, 3); !errors.Is(err, ErrGeneration) {
		t.Fatal("old-generation acknowledgement was accepted", err)
	}
	assertBatchesFrom(t, scanner, 2, 1, 132) // The resumed scan did not re-publish the first 128 objects.
	if _, err := scanner.Ack(context.Background(), 1, 2, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Release(context.Background(), 1, 2, 3); err != nil {
		t.Fatal(err)
	}
	waitBatchesReleased(t, scanner, 1, 3)
	if n := countCleanupRows(t, db, batches, 1); n != 0 {
		t.Fatalf("acknowledged batches retained: %d", n)
	}
}

func TestResumeReportsChangedAndMissingSources(t *testing.T) {
	for _, change := range []string{"replace", "remove"} {
		t.Run(change, func(t *testing.T) {
			root := sourceFiles(t, 130)
			factory := &localFactory{gateAfter: 128, reached: make(chan struct{}), release: make(chan struct{})}
			scanner, _ := newScanner(t, filepath.Join(t.TempDir(), "agent.db"), factory, Options{})
			if _, err := scanner.Start(context.Background(), localSpec(root)); err != nil {
				t.Fatal(err)
			}
			waitReached(t, factory.reached)
			if _, err := scanner.Stop(context.Background(), 1, 1); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "0000.txt")
			if change == "replace" {
				if err := os.WriteFile(path, []byte("changed source bytes"), 0600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if _, err := scanner.Resume(context.Background(), 1, 2); err != nil {
				t.Fatal(err)
			}
			if change == "replace" {
				failed := waitStatus(t, scanner, Failed)
				if !strings.Contains(failed.Error, ErrSourceChanged.Error()) || failed.TotalObjects != 128 || failed.LastBatch != 1 || failed.ScanComplete {
					t.Fatal("changed source was not rejected before modifying the committed batch")
				}
			} else {
				completed := waitStatus(t, scanner, Completed)
				if completed.MissingObjects != 1 || completed.Warning == "" || completed.TotalObjects != 130 {
					t.Fatal("missing source silently disappeared from previously committed work")
				}
				assertBatches(t, scanner, 2, 130)
			}
		})
	}
}

func TestSingleObjectUsesStatAndInclusiveStart(t *testing.T) {
	root := sourceFiles(t, 3)
	stamp := time.Unix(1700000000, 0).UTC()
	for index := 0; index < 3; index++ {
		when := stamp.Add(time.Duration(index) * time.Hour)
		if err := os.Chtimes(filepath.Join(root, fmt.Sprintf("%04d.txt", index)), when, when); err != nil {
			t.Fatal(err)
		}
	}
	factory := &localFactory{}
	scanner, _ := newScanner(t, filepath.Join(t.TempDir(), "agent.db"), factory, Options{})
	spec := localSpec(root)
	spec.SingleKey = "0001.txt"
	spec.ModifiedFrom, spec.ModifiedUntil = stamp.Add(time.Hour), stamp.Add(2*time.Hour)
	if _, err := scanner.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	completed := waitStatus(t, scanner, Completed)
	if completed.TotalObjects != 1 || factory.stats.Load() != 1 || factory.scans.Load() != 0 {
		t.Fatal("single-object discovery scanned a directory or rejected inclusive start boundary")
	}
	assertBatches(t, scanner, 1, 1)
	if resumed, err := scanner.Resume(context.Background(), 1, 2); err != nil || !resumed.ScanComplete || resumed.Generation != 2 || factory.stats.Load() != 1 {
		t.Fatal("resuming an already completed scan rediscovered the source", err)
	}
	assertBatches(t, scanner, 2, 1)
}

func TestBatchByteBoundaryAndSingleLargeObject(t *testing.T) {
	root := t.TempDir()
	for index, size := range []int64{128 << 20, 128 << 20, 128 << 20, 512 << 20} {
		file, err := os.Create(filepath.Join(root, fmt.Sprintf("%04d.bin", index)))
		if err != nil {
			t.Fatal(err)
		}
		err = file.Truncate(size) // Metadata-only fixture; no throughput claim.
		file.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	scanner, _ := newScanner(t, filepath.Join(t.TempDir(), "agent.db"), &localFactory{}, Options{})
	if _, err := scanner.Start(context.Background(), localSpec(root)); err != nil {
		t.Fatal(err)
	}
	completed := waitStatus(t, scanner, Completed)
	if completed.LastBatch != 3 || completed.TotalBytes != 896<<20 {
		t.Fatal("byte-size dispatch boundary is incorrect")
	}
	assertBatches(t, scanner, 1, 4)
}

func TestKeyRangeHasExclusiveStartAndInclusiveEndWithoutEarlyExit(t *testing.T) {
	root := t.TempDir()
	// Directory traversal visits a/z before sibling a.x, although a.x sorts first.
	// A suffix avoids the Windows trailing-dot alias without changing that ordering.
	keys := []string{"a/z", "a-", "a.x", "b", "é", "中", "文", "😀"}
	for _, key := range keys {
		path := filepath.Join(root, filepath.FromSlash(key))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("data"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		name, start, end, prefix string
		want                     []string
	}{
		{"unordered-directory", "a-", "a.x", "", []string{"a.x"}},
		{"both-boundaries", "a-", "b", "", []string{"a.x", "a/z", "b"}},
		{"utf8-byte-order", "é", "文", "", []string{"中", "文"}},
		{"equal-is-empty", "b", "b", "", []string{}},
		{"prefix-intersection", "a-", "b", "a/", []string{"a/z"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			scanner, _ := newScanner(t, filepath.Join(t.TempDir(), "agent.db"), &localFactory{}, Options{})
			spec := localSpec(root)
			spec.StartKey, spec.EndKey, spec.Prefix = test.start, test.end, test.prefix
			if _, err := scanner.Start(context.Background(), spec); err != nil {
				t.Fatal(err)
			}
			completed := waitStatus(t, scanner, Completed)
			if completed.TotalObjects != int64(len(test.want)) {
				t.Fatal("range produced the wrong object count")
			}
			got := []string{}
			for after := int64(0); after < completed.LastBatch; after++ {
				batch, found, err := scanner.Batch(context.Background(), 1, 1, after)
				if err != nil || !found {
					t.Fatal("missing range batch", err)
				}
				for _, object := range batch.Objects {
					got = append(got, object.Key)
				}
			}
			slices.Sort(got)
			if !slices.Equal(got, test.want) {
				t.Fatalf("range got %v, want %v", got, test.want)
			}
		})
	}
	scanner, _ := newScanner(t, filepath.Join(t.TempDir(), "agent.db"), &localFactory{}, Options{})
	for _, spec := range []Spec{
		{ParentID: 1, Generation: 1, Source: localSpec(root).Source, SingleKey: "a.x", StartKey: "a-"},
		{ParentID: 1, Generation: 1, Source: localSpec(root).Source, SingleKey: "a.x", EndKey: "b"},
		{ParentID: 1, Generation: 1, Source: localSpec(root).Source, StartKey: "b", EndKey: "a"},
	} {
		if _, err := scanner.Start(context.Background(), spec); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid key range was accepted", err)
		}
	}
}

func TestDiscoveryCrashProcess(t *testing.T) {
	if os.Getenv("OSS_DISCOVERY_CRASH_HELPER") != "1" {
		return
	}
	factory := &localFactory{gateAfter: 128, reached: make(chan struct{}), release: make(chan struct{})}
	db, err := state.Open(os.Getenv("OSS_DISCOVERY_CRASH_DB"))
	if err != nil {
		t.Fatal(err)
	}
	scanner, err := New(db, factory, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Start(context.Background(), localSpec(os.Getenv("OSS_DISCOVERY_CRASH_SOURCE"))); err != nil {
		t.Fatal(err)
	}
	<-factory.reached
	fmt.Println("checkpoint-committed")
	select {} // Parent forcibly terminates the process, without Close or DB flush.
}

func TestCommittedDiscoverySurvivesActualProcessKill(t *testing.T) {
	root := sourceFiles(t, 130)
	path := filepath.Join(t.TempDir(), "agent.db")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^TestDiscoveryCrashProcess$", "-test.count=1")
	command.Env = append(os.Environ(), "OSS_DISCOVERY_CRASH_HELPER=1", "OSS_DISCOVERY_CRASH_DB="+path, "OSS_DISCOVERY_CRASH_SOURCE="+root)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer command.Process.Kill()
	lines := bufio.NewScanner(stdout)
	if !lines.Scan() || lines.Text() != "checkpoint-committed" {
		t.Fatal("child did not reach a durably committed discovery batch")
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("test child exited normally instead of being killed")
	}
	scanner, _ := newScanner(t, path, &localFactory{}, Options{})
	current, err := scanner.Status(context.Background(), 1)
	if err != nil || current.Status != Recoverable || current.TotalObjects != 128 || current.LastBatch != 1 || current.ScanComplete {
		t.Fatal("crash lost the checkpoint or incorrectly completed discovery", err)
	}
	if _, err := scanner.Start(context.Background(), localSpec(root)); err != nil {
		t.Fatal(err)
	}
	completed := waitStatus(t, scanner, Completed)
	if completed.TotalObjects != 130 || completed.TotalBytes != 1170 || completed.LastBatch != 2 {
		t.Fatal("crash recovery duplicated committed scan work")
	}
	assertBatches(t, scanner, 1, 130)
}

func TestStopBeforeStartRequiresNewGenerationAndFullSpec(t *testing.T) {
	root := sourceFiles(t, 1)
	factory := &localFactory{}
	scanner, _ := newScanner(t, filepath.Join(t.TempDir(), "agent.db"), factory, Options{})
	if _, err := scanner.Stop(context.Background(), 1, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Start(context.Background(), localSpec(root)); !errors.Is(err, ErrStopped) {
		t.Fatal("late first Start bypassed the stored stop fence", err)
	}
	for range 2 {
		resumed, err := scanner.Resume(context.Background(), 1, 2)
		if err != nil || resumed.Status != Recoverable || resumed.Generation != 2 || factory.opens.Load() != 0 {
			t.Fatal("resuming an empty fence did not wait for its full specification", err)
		}
	}
	spec := localSpec(root)
	spec.Generation = 2
	if _, err := scanner.Start(context.Background(), spec); err != nil {
		t.Fatal("full specification could not fill the resumed fence", err)
	}
	waitStatus(t, scanner, Completed)
	if _, err := scanner.Start(context.Background(), localSpec(root)); !errors.Is(err, ErrGeneration) {
		t.Fatal("old Start was accepted after a new generation filled the fence", err)
	}
	spec.Prefix = "other"
	if _, err := scanner.Start(context.Background(), spec); !errors.Is(err, ErrConflict) {
		t.Fatal("immutable scan specification was replaced", err)
	}
}

func TestDatabaseFailureIsReportedAndCommittedBatchRemainsRecoverable(t *testing.T) {
	root := sourceFiles(t, 130)
	path := filepath.Join(t.TempDir(), "agent.db")
	factory := &localFactory{gateAfter: 128, reached: make(chan struct{}), release: make(chan struct{})}
	scanner, db := newScanner(t, path, factory, Options{})
	if _, err := scanner.Start(context.Background(), localSpec(root)); err != nil {
		t.Fatal(err)
	}
	waitReached(t, factory.reached)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := scanner.Close(); err == nil || !strings.Contains(err.Error(), "persist discovery state") {
		t.Fatal("terminal-state persistence failure was swallowed", err)
	}
	if _, err := scanner.Status(context.Background(), 1); err == nil {
		t.Fatal("closed state store was reported as healthy")
	}
	recovered, _ := newScanner(t, path, &localFactory{}, Options{})
	current, err := recovered.Status(context.Background(), 1)
	if err != nil || current.Status != Recoverable || current.TotalObjects != 128 || current.LastBatch != 1 {
		t.Fatal("failure damaged the previously committed batch", err)
	}
	if _, err := recovered.Start(context.Background(), localSpec(root)); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, recovered, Completed)
	assertBatches(t, recovered, 1, 130)
}

func TestConcurrentDuplicateStartsUseOneScanner(t *testing.T) {
	root := sourceFiles(t, 130)
	factory := &localFactory{gateAfter: 1, reached: make(chan struct{}), release: make(chan struct{})}
	scanner, _ := newScanner(t, filepath.Join(t.TempDir(), "agent.db"), factory, Options{})
	var wg sync.WaitGroup
	errorsFound := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := scanner.Start(context.Background(), localSpec(root))
			errorsFound <- err
		}()
	}
	wg.Wait()
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatal(err)
		}
	}
	waitReached(t, factory.reached)
	if factory.opens.Load() != 1 {
		t.Fatal("concurrent duplicate starts created multiple scans")
	}
	close(factory.release)
	waitStatus(t, scanner, Completed)
}
