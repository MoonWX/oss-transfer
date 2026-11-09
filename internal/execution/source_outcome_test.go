package execution

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// An object deleted at its source after discovery is skipped, not failed. An
// object changed after discovery fails, and a retry of that failure copies the
// source's current version, pinned durably, with consistent counters.
// agentFixture models an S3 source with the fixture's local files, as an
// Agent database would hold it.
func agentFixture(t *testing.T, count int) *fixture {
	f := newFixture(t, count)
	if err := f.db.ClaimRole("agent"); err != nil {
		t.Fatal(err)
	}
	f.job.Source.Kind = "S3"
	return f
}

func TestSourceDeletedIsSkippedAndChangedSourceIsReadAgainOnRetry(t *testing.T) {
	f := agentFixture(t, 3)
	deleted, changed := f.job.Items[1], f.job.Items[2]
	if err := os.Remove(filepath.Join(f.src, deleted.Source.Key)); err != nil {
		t.Fatal(err)
	}
	current := []byte("a newer version with another size")
	if err := os.WriteFile(filepath.Join(f.src, changed.Source.Key), current, 0600); err != nil {
		t.Fatal(err)
	}
	f.startEngine(nil, Options{})
	f.acceptStart()
	ended := f.wait()
	if ended.Status != PartialFailed || ended.Counters.Succeeded != 1 || ended.Counters.Skipped != 1 || ended.Counters.Failed != 1 {
		t.Fatalf("first attempt: %+v", ended.Counters)
	}
	gone := ended.Results[1]
	if gone.Status != ItemSkipped || gone.Reconciliation != ReasonSourceDeleted || gone.Error != "" || gone.Target.Key != deleted.DestinationKey || gone.ResolvedSource != nil {
		t.Fatalf("deleted source: %+v", gone)
	}
	failed := ended.Results[2]
	if failed.Status != ItemFailed || failed.Reconciliation != ReasonSourceChanged || !strings.Contains(failed.Error, "source changed") {
		t.Fatalf("changed source: %+v", failed)
	}
	if _, err := os.Stat(filepath.Join(f.dst, changed.DestinationKey)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a changed source was published on the first attempt: %v", err)
	}
	reopened, ok, err := f.engine.RetryFailed(bg, f.job.ChildID, 1, ended.Attempt)
	if err != nil || !ok || reopened.Results[2].Status != ItemPending || reopened.Results[2].Reconciliation != ReasonSourceChanged {
		t.Fatalf("retry did not ask to read the source again: %+v %v %v", reopened.Results[2], ok, err)
	}
	if err := reopened.ValidateResolvedSources(false, &ended); err != nil {
		t.Fatal(err)
	}
	if err := f.engine.Start(bg, f.job.ChildID, 1); err != nil {
		t.Fatal(err)
	}
	done := f.wait()
	retried := done.Results[2]
	if done.Status != Completed || retried.Status != ItemSucceeded || retried.Reconciliation != ReasonSourceRefreshed || retried.ResolvedSource == nil || retried.ResolvedSource.Size != int64(len(current)) {
		t.Fatalf("retry of a changed source: %+v %+v", done.Status, retried)
	}
	want := Counters{Total: 3, TotalBytes: deleted.Source.Size + int64(len(current)), Succeeded: 2, SucceededBytes: int64(len(current)), Skipped: 1, SkippedBytes: deleted.Source.Size}
	if done.Counters != want {
		t.Fatalf("counters %+v, want %+v", done.Counters, want)
	}
	if got, err := os.ReadFile(filepath.Join(f.dst, changed.DestinationKey)); err != nil || !bytes.Equal(got, current) {
		t.Fatalf("current version not copied: %q %v", got, err)
	}
	if err := done.ValidateResolvedSources(false, &reopened); err != nil {
		t.Fatal(err)
	}
	// A later resume revalidates credited objects; a deleted source has no
	// target to check and stays skipped, the refreshed object stays pinned.
	if err := f.engine.Resume(bg, f.job.ChildID, 1, false); err != nil {
		t.Fatal(err)
	}
	resumed := f.wait()
	if resumed.Status != Completed || resumed.Counters != want || resumed.Results[1].Reconciliation != ReasonSourceDeleted || !samePersistedSourceValue(resumed.Results[2].ResolvedSource, retried.ResolvedSource) {
		t.Fatalf("resume changed the outcomes: %+v", resumed)
	}
}

func TestRetryOfChangedSourceSkipsObjectDeletedSinceItFailed(t *testing.T) {
	f := agentFixture(t, 2)
	key := f.job.Items[1].Source.Key
	if err := os.WriteFile(filepath.Join(f.src, key), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	f.startEngine(nil, Options{})
	f.acceptStart()
	ended := f.wait()
	if ended.Results[1].Reconciliation != ReasonSourceChanged {
		t.Fatalf("first attempt: %+v", ended.Results[1])
	}
	if err := os.Remove(filepath.Join(f.src, key)); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := f.engine.RetryFailed(bg, f.job.ChildID, 1, ended.Attempt); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := f.engine.Start(bg, f.job.ChildID, 1); err != nil {
		t.Fatal(err)
	}
	done := f.wait()
	if done.Status != Completed || done.Results[1].Status != ItemSkipped || done.Results[1].Reconciliation != ReasonSourceDeleted || done.Results[1].ResolvedSource != nil {
		t.Fatalf("deleted after the failure: %+v", done)
	}
	if err := done.ValidateResolvedSources(false, &ended); err != nil {
		t.Fatal(err)
	}
}

// vanishingSource opens normally and then reports the object missing while
// the target reads it, as a ranged fetch of a deleted object does.
type vanishingSource struct {
	transferstore.Store
	err error
}

func (s *vanishingSource) Open(ctx context.Context, key string, opts transferstore.OpenOptions) (io.ReadCloser, transferstore.Object, error) {
	reader, object, err := s.Store.Open(ctx, key, opts)
	if err != nil {
		return nil, object, err
	}
	return struct {
		io.Reader
		io.Closer
	}{io.MultiReader(io.LimitReader(reader, 1), errReader{s.err}), reader}, object, nil
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// disappearingTarget fails a write as a target does when a destination it
// found disappears during a conditional write: with ErrSourceChanged, which
// must not be mistaken for a changed source.
type disappearingTarget struct{ transferstore.Store }

func (t disappearingTarget) Write(ctx context.Context, key string, input io.Reader, options transferstore.WriteOptions) (transferstore.WriteResult, error) {
	_, _ = io.Copy(io.Discard, input)
	return transferstore.WriteResult{}, fmt.Errorf("%w: concurrent destination disappeared", transferstore.ErrSourceChanged)
}

func TestSourceErrorsWhileTheTargetReadsAreClassifiedBySide(t *testing.T) {
	for _, name := range []string{"deleted", "changed", "target"} {
		t.Run(name, func(t *testing.T) {
			f := agentFixture(t, 2)
			f.job.Items = f.job.Items[1:]
			base := f.factory()
			f.startEngine(StoreFactoryFunc(func(ctx context.Context, j Job) (transferstore.Store, transferstore.Store, error) {
				s, d, err := base.Open(ctx, j)
				switch name {
				case "deleted":
					return &vanishingSource{Store: s, err: fmt.Errorf("%w: range fetch", transferstore.ErrNotFound)}, d, err
				case "changed":
					return &vanishingSource{Store: s, err: transferstore.ErrSourceChanged}, d, err
				}
				return s, disappearingTarget{d}, err
			}), Options{})
			f.acceptStart()
			result := f.wait().Results[0]
			switch name {
			case "deleted":
				if result.Status != ItemSkipped || result.Reconciliation != ReasonSourceDeleted {
					t.Fatalf("source deleted while read: %+v", result)
				}
			case "changed":
				if result.Status != ItemFailed || result.Reconciliation != ReasonSourceChanged {
					t.Fatalf("source changed while read: %+v", result)
				}
			default:
				if result.Status != ItemFailed || result.Reconciliation != "" || !strings.Contains(result.Error, "write destination") {
					t.Fatalf("target failure: %+v", result)
				}
			}
			if _, err := os.Stat(filepath.Join(f.dst, f.job.Items[0].DestinationKey)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("destination published: %v", err)
			}
		})
	}
}

// A missing local source file still fails: the volume may not be mounted.
func TestMissingLocalSourceFileStillFails(t *testing.T) {
	f := newFixture(t, 2)
	if err := os.Remove(filepath.Join(f.src, f.job.Items[1].Source.Key)); err != nil {
		t.Fatal(err)
	}
	f.startEngine(nil, Options{})
	f.acceptStart()
	if done := f.wait(); done.Status != PartialFailed || done.Results[1].Status != ItemFailed || done.Results[1].Reconciliation != "" {
		t.Fatalf("missing local file: %+v", done.Results[1])
	}
}

// A missing inventory key never had an identity: it still fails, so the
// operator can supply it and retry.
func TestMissingInventoryKeyIsNotTreatedAsDeleted(t *testing.T) {
	f := missingInventoryFixture(t)
	f.startEngine(nil, Options{})
	if _, err := f.engine.Accept(bg, f.job, true); err != nil {
		t.Fatal(err)
	}
	if err := f.engine.Start(bg, f.job.ChildID, 1); err != nil {
		t.Fatal(err)
	}
	if done := f.wait(); done.Results[1].Status != ItemFailed || done.Results[1].Reconciliation != "" {
		t.Fatalf("missing inventory key: %+v", done.Results[1])
	}
}

func TestResolutionValidationAllowsOnlyExplainedRefreshes(t *testing.T) {
	item := Item{ID: "i", Source: transferstore.Object{Key: "k", Size: 3, ETag: "old"}}
	pin := transferstore.Object{Key: "k", Size: 9, ETag: "new"}
	previous := Record{Job: Job{Source: Endpoint{Kind: "S3"}, Items: []Item{item}}, Results: []ItemResult{{ID: "i", Status: ItemPending, Reconciliation: ReasonSourceChanged, ResolvedSource: &pin}}}
	for _, test := range []struct {
		name   string
		result ItemResult
		valid  bool
	}{
		{"refreshed", ItemResult{ID: "i", Status: ItemSucceeded, Reconciliation: ReasonSourceRefreshed, ResolvedSource: &pin}, true},
		{"changed again", ItemResult{ID: "i", Status: ItemPending, Reconciliation: ReasonSourceChanged, ResolvedSource: &transferstore.Object{Key: "k", Size: 1, ETag: "newer"}}, true},
		{"deleted", ItemResult{ID: "i", Status: ItemSkipped, Reconciliation: ReasonSourceDeleted, ResolvedSource: &pin}, true},
		{"unexplained change", ItemResult{ID: "i", Status: ItemSucceeded, ResolvedSource: &transferstore.Object{Key: "k", Size: 1, ETag: "newer"}}, false},
		{"removed", ItemResult{ID: "i", Status: ItemSucceeded, Reconciliation: ReasonSourceRefreshed}, false},
		{"other key", ItemResult{ID: "i", Status: ItemSucceeded, Reconciliation: ReasonSourceRefreshed, ResolvedSource: &transferstore.Object{Key: "other", Size: 1, ETag: "x"}}, false},
		{"no identity", ItemResult{ID: "i", Status: ItemSucceeded, Reconciliation: ReasonSourceRefreshed, ResolvedSource: &transferstore.Object{Key: "k", Size: 1}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := previous
			current.Results = []ItemResult{test.result}
			if err := current.ValidateResolvedSources(false, &previous); (err == nil) != test.valid {
				t.Fatalf("valid=%v: %v", test.valid, err)
			}
		})
	}
	// Every path that returns an object to pending keeps what allows its pin.
	for reason, want := range map[string]string{ReasonSourceChanged: ReasonSourceChanged, ReasonSourceRefreshed: ReasonSourceRefreshed, ReasonSourceDeleted: ReasonSourceRefreshed, "": ""} {
		if got := PendingResult(ItemResult{ID: "i", Status: ItemFailed, Error: "x", Reconciliation: reason, ResolvedSource: &pin}); got.Reconciliation != want || got.Status != ItemPending || got.Error != "" || got.ResolvedSource != &pin {
			t.Fatalf("%q: %+v", reason, got)
		}
	}
	if got := PendingResult(ItemResult{ID: "i", Status: ItemSucceeded, Reconciliation: ReasonSourceRefreshed}); got.Reconciliation != "" {
		t.Fatalf("an unpinned result kept a refresh reason: %+v", got)
	}
}
