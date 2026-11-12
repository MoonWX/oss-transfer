package execution

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// writeOnlyTarget answers every HEAD with HTTP 403, as a target key without
// s3:GetObject does, and counts the writes.
type writeOnlyTarget struct {
	transferstore.Store
	writes *atomic.Int64
}

func (s writeOnlyTarget) Stat(context.Context, string) (transferstore.Object, error) {
	return transferstore.Object{}, &awshttp.ResponseError{ResponseError: &smithyhttp.ResponseError{
		Response: &smithyhttp.Response{Response: &http.Response{StatusCode: http.StatusForbidden}},
		Err:      errors.New("api error Forbidden: Forbidden"),
	}}
}

func (s writeOnlyTarget) Write(ctx context.Context, key string, input io.Reader, options transferstore.WriteOptions) (transferstore.WriteResult, error) {
	s.writes.Add(1)
	return s.Store.Write(ctx, key, input, options)
}

func resumeWithWriteOnlyTarget(t *testing.T, policy string) (Record, int64) {
	t.Helper()
	f := newFixture(t, 3)
	f.job.ConflictPolicy = policy
	f.startEngine(nil, Options{WorkersPerJob: 2})
	f.acceptStart()
	if record := f.wait(); record.Status != Completed {
		t.Fatalf("initial transfer: %s %s", record.Status, record.Error)
	}
	var writes atomic.Int64
	factory := f.factory()
	f.engine.factory = StoreFactoryFunc(func(ctx context.Context, job Job) (transferstore.Store, transferstore.Store, error) {
		source, target, err := factory.Open(ctx, job)
		return source, writeOnlyTarget{Store: target, writes: &writes}, err
	})
	if err := f.engine.Resume(bg, f.job.ChildID, 1, false); err != nil {
		t.Fatal(err)
	}
	return f.wait(), writes.Load()
}

// Under "overwrite", recovery cannot read back targets a write-only key
// wrote: it writes them again instead of failing the child.
func TestRecoveryRewritesUnreadableTargetsWhenOverwriting(t *testing.T) {
	record, writes := resumeWithWriteOnlyTarget(t, ConflictOverwrite)
	if record.Status != Completed || record.Counters.Succeeded != 3 || writes != 3 {
		t.Fatalf("status %s, succeeded %d, rewrites %d: %s", record.Status, record.Counters.Succeeded, writes, record.Error)
	}
}

// Other policies need HEAD to decide, so a denied HEAD still fails recovery
// instead of overwriting objects the policy would keep.
func TestRecoveryStillFailsOnUnreadableTargetsWhenSkipping(t *testing.T) {
	record, writes := resumeWithWriteOnlyTarget(t, ConflictSkip)
	if record.Status == Completed || writes != 0 {
		t.Fatalf("status %s, rewrites %d", record.Status, writes)
	}
}
