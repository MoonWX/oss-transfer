package discovery

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// limitGate holds each source scan before it reads anything until the test
// releases that source, and keeps the context the scan runs under.
type limitGate struct {
	mu       sync.Mutex
	release  map[string]chan struct{}
	contexts map[string]context.Context
	entered  chan string
}

type limitGateStore struct {
	transferstore.Store
	held *limitGate
	root string
}

func newLimitGate() *limitGate {
	return &limitGate{release: map[string]chan struct{}{}, contexts: map[string]context.Context{}, entered: make(chan string, 16)}
}

// source creates a source directory whose scan holds until finish.
func (h *limitGate) source(t *testing.T) string {
	t.Helper()
	root := sourceFiles(t, 3)
	h.mu.Lock()
	h.release[root] = make(chan struct{})
	h.mu.Unlock()
	return root
}

func (h *limitGate) finish(root string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	close(h.release[root])
}

func (h *limitGate) context(root string) context.Context {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.contexts[root]
}

// waitEntered returns the source of the next scan that started reading.
func (h *limitGate) waitEntered(t *testing.T) string {
	t.Helper()
	select {
	case root := <-h.entered:
		return root
	case <-time.After(2 * time.Minute): // hang guard only
		t.Fatal("no scan reached its source")
		return ""
	}
}

func (h *limitGate) OpenEndpoint(ctx context.Context, endpoint execution.Endpoint) (transferstore.Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	store, err := transferstore.NewLocal(endpoint.Root)
	if err != nil {
		return nil, err
	}
	return &limitGateStore{Store: store, held: h, root: endpoint.Root}, nil
}

func (s *limitGateStore) Scan(ctx context.Context, options transferstore.ListOptions, emit func(transferstore.Object) error) error {
	s.held.mu.Lock()
	s.held.contexts[s.root] = ctx
	release := s.held.release[s.root]
	s.held.mu.Unlock()
	s.held.entered <- s.root
	select {
	case <-release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.Store.Scan(ctx, options, emit)
}

func limitSpec(parentID int64, root string) Spec {
	return Spec{ParentID: parentID, Generation: 1, Source: execution.Endpoint{Kind: "LOCAL", Root: root}}
}

// waitLimitScanEnded waits until the parent's scan run has ended and left the
// running set, as the run's own exit does.
func waitLimitScanEnded(t *testing.T, scanner *Scanner, parentID int64) {
	t.Helper()
	scanner.mu.Lock()
	run := scanner.active[parentID]
	scanner.mu.Unlock()
	if run != nil {
		select {
		case <-run.done:
		case <-time.After(2 * time.Minute): // hang guard only
			t.Fatalf("the scan of parent %d did not end", parentID)
		}
	}
	scanner.mu.Lock()
	scanner.isActiveLocked(parentID)
	scanner.mu.Unlock()
}

func assertLimitScanCompleted(t *testing.T, scanner *Scanner, parentID int64) {
	t.Helper()
	current, err := scanner.Status(context.Background(), parentID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != Completed || current.TotalObjects != 3 {
		t.Fatalf("parent %d: status %s with %d objects (%s), want a completed scan of 3", parentID, current.Status, current.TotalObjects, current.Error)
	}
}

// Lowering the scan concurrency below the running scans cancels none of
// them; new scans start only once fewer scans than the new value run.
func TestLoweringScanConcurrencyCancelsNothing(t *testing.T) {
	ctx := context.Background()
	held := newLimitGate()
	scanner, _ := newScanner(t, filepath.Join(t.TempDir(), "agent.db"), held, Options{MaxConcurrentScans: 3})
	roots := map[int64]string{}
	for id := int64(1); id <= 4; id++ {
		roots[id] = held.source(t)
	}
	for id := int64(1); id <= 3; id++ {
		if _, err := scanner.Start(ctx, limitSpec(id, roots[id])); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		held.waitEntered(t)
	}
	if err := scanner.SetConcurrency(1); err != nil {
		t.Fatal(err)
	}
	for id := int64(1); id <= 3; id++ {
		if err := held.context(roots[id]).Err(); err != nil {
			t.Fatalf("lowering the scan concurrency canceled the scan of parent %d: %v", id, err)
		}
	}
	if active, capacity := scanner.Concurrency(); active != 3 || capacity != 1 {
		t.Fatalf("concurrency %d of %d, want 3 running over a limit of 1", active, capacity)
	}
	if _, err := scanner.Start(ctx, limitSpec(4, roots[4])); !errors.Is(err, ErrCapacity) {
		t.Fatalf("a new scan started while more scans than the limit run: %v", err)
	}
	if _, err := scanner.Status(ctx, 4); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a refused scan left state: %v", err)
	}
	// One scan still runs after two finish, which the limit of 1 fills.
	for id := int64(1); id <= 2; id++ {
		held.finish(roots[id])
		waitLimitScanEnded(t, scanner, id)
		assertLimitScanCompleted(t, scanner, id)
	}
	if _, err := scanner.Start(ctx, limitSpec(4, roots[4])); !errors.Is(err, ErrCapacity) {
		t.Fatalf("a new scan started while the remaining scan fills the limit: %v", err)
	}
	held.finish(roots[3])
	waitLimitScanEnded(t, scanner, 3)
	assertLimitScanCompleted(t, scanner, 3)
	if _, err := scanner.Start(ctx, limitSpec(4, roots[4])); err != nil {
		t.Fatalf("a new scan was refused once no scan ran: %v", err)
	}
	if root := held.waitEntered(t); root != roots[4] {
		t.Fatalf("unexpected scan %s started", root)
	}
	held.finish(roots[4])
	waitLimitScanEnded(t, scanner, 4)
	assertLimitScanCompleted(t, scanner, 4)
}

// Raising the scan concurrency admits a waiting scan at once.
func TestRaisingScanConcurrencyAdmitsAtOnce(t *testing.T) {
	ctx := context.Background()
	held := newLimitGate()
	scanner, _ := newScanner(t, filepath.Join(t.TempDir(), "agent.db"), held, Options{MaxConcurrentScans: 1})
	first, second := held.source(t), held.source(t)
	if _, err := scanner.Start(ctx, limitSpec(1, first)); err != nil {
		t.Fatal(err)
	}
	held.waitEntered(t)
	if _, err := scanner.Start(ctx, limitSpec(2, second)); !errors.Is(err, ErrCapacity) {
		t.Fatalf("a second scan started over a limit of 1: %v", err)
	}
	if err := scanner.SetConcurrency(2); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Start(ctx, limitSpec(2, second)); err != nil {
		t.Fatalf("the raised limit did not admit the second scan: %v", err)
	}
	held.waitEntered(t)
	if active, capacity := scanner.Concurrency(); active != 2 || capacity != 2 {
		t.Fatalf("concurrency %d of %d, want 2 of 2", active, capacity)
	}
	if err := held.context(first).Err(); err != nil {
		t.Fatalf("raising the limit canceled the first scan: %v", err)
	}
	held.finish(first)
	held.finish(second)
	waitLimitScanEnded(t, scanner, 1)
	waitLimitScanEnded(t, scanner, 2)
	assertLimitScanCompleted(t, scanner, 1)
	assertLimitScanCompleted(t, scanner, 2)
}

func TestSetConcurrencyRejectsValuesOutsideTheBounds(t *testing.T) {
	scanner, _ := newScanner(t, filepath.Join(t.TempDir(), "agent.db"), newLimitGate(), Options{})
	for _, invalid := range []int{0, -1, 17} {
		if err := scanner.SetConcurrency(invalid); !errors.Is(err, ErrInvalid) {
			t.Fatalf("scan concurrency %d accepted: %v", invalid, err)
		}
	}
	if _, capacity := scanner.Concurrency(); capacity != 2 {
		t.Fatalf("a refused value changed the limit to %d", capacity)
	}
	for _, valid := range []int{1, 16} {
		if err := scanner.SetConcurrency(valid); err != nil {
			t.Fatal(err)
		}
		if _, capacity := scanner.Concurrency(); capacity != valid {
			t.Fatalf("limit %d, want %d", capacity, valid)
		}
	}
}
