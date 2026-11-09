package execution

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// failingWrite fails every write as a target outage that outlasted the
// retries would, with a signed-looking URL in the error.
type failingWrite struct{ transferstore.Store }

func (failingWrite) Write(context.Context, string, io.Reader, transferstore.WriteOptions) (transferstore.WriteResult, error) {
	return transferstore.WriteResult{}, errors.New(`operation error S3: PutObject, exceeded maximum number of attempts, 10, request send failed, Put "http://target:9000/bucket/key?x-id=PutObject&X-Amz-Signature=secret": EOF`)
}

// The service log shows each child attempt's start and end with its counts
// and the first object failures, with their keys and causes, and counts the
// rest instead of writing millions of lines.
func TestEngineLogsAttemptsAndCapsObjectFailures(t *testing.T) {
	f := newFixture(t, LoggedObjectFailures+5)
	var mu sync.Mutex
	var lines []string
	base := f.factory()
	f.startEngine(StoreFactoryFunc(func(ctx context.Context, job Job) (transferstore.Store, transferstore.Store, error) {
		source, target, err := base.Open(ctx, job)
		return source, failingWrite{target}, err
	}), Options{Logf: func(format string, args ...any) {
		mu.Lock()
		lines = append(lines, fmt.Sprintf(format, args...))
		mu.Unlock()
	}})
	f.acceptStart()
	record := f.wait()
	if record.Counters.Failed != int64(len(f.job.Items)) {
		t.Fatalf("counters %+v", record.Counters)
	}
	mu.Lock()
	defer mu.Unlock()
	total := len(f.job.Items)
	if len(lines) != LoggedObjectFailures+3 {
		t.Fatalf("service log:\n%s", strings.Join(lines, "\n"))
	}
	want := []string{
		fmt.Sprintf("task #224-1 (child 224-1): attempt 1 started: %d of %d objects to transfer (0 succeeded, 0 skipped, 0 failed before)", total, total),
		"task #224-1 (child 224-1): 5 more object failures not logged; the task's exception records list them all",
		fmt.Sprintf("task #224-1 (child 224-1): attempt 1 ended FAILED: 0 succeeded, 0 skipped, %d failed of %d objects", total, total),
	}
	if got := []string{lines[0], lines[len(lines)-2], lines[len(lines)-1]}; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("service log:\n%s", strings.Join(lines, "\n"))
	}
	for _, line := range lines[1 : len(lines)-2] {
		// The cause keeps its stage and URL, without the query.
		if !strings.HasPrefix(line, `task #224-1 (child 224-1): object "file-`) || !strings.Contains(line, `failed: write destination "file-`) || !strings.Contains(line, `bucket/key?…": EOF`) || strings.Contains(line, "Signature") {
			t.Errorf("object failure line: %q", line)
		}
	}
}
