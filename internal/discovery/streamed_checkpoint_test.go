package discovery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// A page whose cursor names a streamed directory records that resuming it
// needs the scan protocol; other pages do not.
func TestCheckpointRecordsTheProtocolAStreamedCursorNeeds(t *testing.T) {
	scanner, _, attempt := commitFixture(t)
	ctx := context.Background()
	if err := scanner.beginCheckpoint(ctx, &attempt); err != nil {
		t.Fatal(err)
	}
	for _, streamed := range []bool{true, false} {
		marker := Checkpoint{Version: 1, Pass: attempt.Checkpoint.Pass, Cursor: fmt.Sprint("page-", streamed)}
		if err := scanner.commitBatches(ctx, attempt, []pendingBatch{{checkpoint: &marker, streamed: streamed}}); err != nil {
			t.Fatal(err)
		}
		current, err := scanner.Status(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		if streamed {
			want = 1
		}
		if current.TransferCheckpoint == nil || current.TransferCheckpoint.RequiresScanProtocol != want {
			t.Fatalf("streamed=%v: %+v", streamed, current.TransferCheckpoint)
		}
	}
}

// streamedPagesFactory opens LOCAL sources whose pages, all but the last,
// name a streamed directory, as the pages of an Agent that streams long
// directories do.
type streamedPagesFactory struct{}

type streamedPagesStore struct{ *transferstore.Local }

func (streamedPagesFactory) OpenEndpoint(_ context.Context, endpoint execution.Endpoint) (transferstore.Store, error) {
	local, err := transferstore.NewLocal(endpoint.Root)
	if err != nil {
		return nil, err
	}
	return &streamedPagesStore{Local: local}, nil
}

func (s *streamedPagesStore) ScanPages(ctx context.Context, options transferstore.ListOptions, emit func(transferstore.Page) error) error {
	options.Limit = 2
	return s.Local.ScanPages(ctx, options, func(page transferstore.Page) error {
		page.Streamed = page.NextCursor != ""
		return emit(page)
	})
}

// The scan passes each page's mark on to its recovery checkpoint, which the
// batches carry to Master.
func TestScanRecordsTheProtocolOfStreamedPages(t *testing.T) {
	root := t.TempDir()
	for i := range 5 {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("%d.bin", i)), []byte("data"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	scanner, _ := newScanner(t, filepath.Join(t.TempDir(), "agent.db"), streamedPagesFactory{}, Options{MaxBatchObjects: 1})
	ctx := context.Background()
	if _, err := scanner.Start(ctx, localSpec(root)); err != nil {
		t.Fatal(err)
	}
	done := waitStatus(t, scanner, Completed)
	if done.TransferCheckpoint == nil || !done.TransferCheckpoint.SourceDone || done.TransferCheckpoint.RequiresScanProtocol != 0 {
		t.Fatalf("final checkpoint %+v", done.TransferCheckpoint)
	}
	required := 0
	for after := int64(0); after < done.LastBatch; after++ {
		batch, found, err := scanner.Batch(ctx, 1, 1, after)
		if err != nil || !found {
			t.Fatalf("batch after %d: %v", after, err)
		}
		if checkpoint := batch.Checkpoint; checkpoint != nil && !checkpoint.SourceDone && checkpoint.RequiresScanProtocol == 1 {
			required++
		}
	}
	if required == 0 {
		t.Fatal("no batch carried a checkpoint that requires the scan protocol")
	}
}
