package execution

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// An ended child reopens in its own generation: only the failed object runs
// again, credited objects keep their ledger, a replay of the same request is
// a no-op and a parent stop fences both a queued and a later reopen.
func TestRetryFailedReopensEndedChildInItsGeneration(t *testing.T) {
	f := newFixture(t, 3)
	var fail atomic.Bool
	fail.Store(true)
	var opens atomic.Int64
	base := f.factory()
	f.startEngine(StoreFactoryFunc(func(ctx context.Context, j Job) (transferstore.Store, transferstore.Store, error) {
		s, d, err := base.Open(ctx, j)
		return &failedSource{Store: s, fail: &fail, opens: &opens}, d, err
	}), Options{})
	f.acceptStart()
	ended := f.wait()
	if ended.Status != PartialFailed || ended.Counters.Succeeded != 2 || ended.Counters.Failed != 1 {
		t.Fatalf("fixture did not end with one failed object: %+v", ended)
	}
	unfinished, _, err := f.repo.Unfinished()
	if err != nil || unfinished != 0 {
		t.Fatal("ended child still counted as pending", unfinished, err)
	}
	if record, reopened, err := f.engine.RetryFailed(bg, f.job.ChildID, 1, ended.Attempt-1); err != nil || reopened || record.Status != PartialFailed || record.Attempt != ended.Attempt {
		t.Fatal("an older observation reopened the child", record, reopened, err)
	}
	if _, _, err := f.engine.RetryFailed(bg, f.job.ChildID, 2, ended.Attempt); !errors.Is(err, ErrGeneration) {
		t.Fatal("another generation was accepted", err)
	}
	fail.Store(false)
	// The engine has no scheduler of its own: the reopened child waits in the
	// ready queue until it is started below.
	reopenedRecord, reopened, err := f.repo.retryFailed(bg, f.job.ChildID, 1, ended.Attempt)
	if err != nil || !reopened {
		t.Fatal("ended child was not reopened", reopened, err)
	}
	if reopenedRecord.Status != Waiting || reopenedRecord.Attempt != ended.Attempt+1 || reopenedRecord.Counters.Succeeded != 2 || reopenedRecord.Counters.Failed != 0 ||
		reopenedRecord.Results[0].Status != ItemSucceeded || reopenedRecord.Results[1].Status != ItemPending || reopenedRecord.Results[2].Status != ItemSucceeded {
		t.Fatalf("reopen changed credited results or kept the failure: %+v", reopenedRecord)
	}
	if unfinished, _, err = f.repo.Unfinished(); err != nil || unfinished != 1 {
		t.Fatal("reopened child bypassed the pending-job bound", unfinished, err)
	}
	pending, err := f.repo.Pending(bg, "", 16)
	if err != nil || len(pending) != 1 || pending[0].Job.ChildID != f.job.ChildID {
		t.Fatal("reopened child is not in the ready queue", pending, err)
	}
	// A replay of the same observation changes nothing.
	if record, again, err := f.engine.RetryFailed(bg, f.job.ChildID, 1, ended.Attempt); err != nil || again || record.Attempt != reopenedRecord.Attempt {
		t.Fatal("replayed retry reopened the child twice", record, again, err)
	}
	if err := f.engine.Start(bg, f.job.ChildID, 1); err != nil {
		t.Fatal(err)
	}
	done := f.wait()
	if done.Status != Completed || done.Counters.Succeeded != 3 || done.Counters.Failed != 0 || opens.Load() != 4 {
		t.Fatalf("retry repeated credited objects or did not finish: %+v opens=%d", done, opens.Load())
	}
	f.assertFiles()
	if _, again, err := f.engine.RetryFailed(bg, f.job.ChildID, 1, done.Attempt); err != nil || again {
		t.Fatal("a completed child was reopened", again, err)
	}
}

func TestRetryFailedIsFencedByParentStop(t *testing.T) {
	f := newFixture(t, 2)
	var fail atomic.Bool
	fail.Store(true)
	var opens atomic.Int64
	base := f.factory()
	f.startEngine(StoreFactoryFunc(func(ctx context.Context, j Job) (transferstore.Store, transferstore.Store, error) {
		s, d, err := base.Open(ctx, j)
		return &failedSource{Store: s, fail: &fail, opens: &opens}, d, err
	}), Options{})
	f.acceptStart()
	ended := f.wait()
	if ended.Status != PartialFailed {
		t.Fatalf("fixture: %+v", ended)
	}
	reopenedRecord, reopened, err := f.repo.retryFailed(bg, f.job.ChildID, 1, ended.Attempt)
	if err != nil || !reopened {
		t.Fatal(reopened, err)
	}
	if err := f.engine.StopParent(bg, f.job.ParentID, 1); err != nil {
		t.Fatal(err)
	}
	stopped, err := f.repo.Get(bg, f.job.ChildID)
	if err != nil || stopped.Status != Stopped || stopped.Attempt <= reopenedRecord.Attempt || stopped.Counters.Failed != 0 || stopped.Counters.Succeeded != 1 {
		t.Fatalf("stop did not fence the reopened child: %+v %v", stopped, err)
	}
	if err := f.engine.Start(bg, f.job.ChildID, 1); !errors.Is(err, ErrStopped) {
		t.Fatal("stopped reopen was admitted", err)
	}
	if _, _, err := f.engine.RetryFailed(bg, f.job.ChildID, 1, stopped.Attempt); !errors.Is(err, ErrStopped) {
		t.Fatal("retry after stop was accepted", err)
	}
	if opens.Load() != 2 {
		t.Fatal("fenced retry opened the source", opens.Load())
	}
}
