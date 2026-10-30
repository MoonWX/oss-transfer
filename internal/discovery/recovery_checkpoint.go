package discovery

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func (c RecoveryCheckpoint) Valid() bool {
	return c.Version == 1 && c.Sequence > 0 && c.ThroughBatch >= 0 && c.Objects >= c.ThroughBatch && c.Bytes >= 0 &&
		len(c.Cursor) <= 32768 && (c.SourceDone && c.Cursor == "" || !c.SourceDone && c.Cursor != "")
}

func sameRecoveryCheckpoint(left, right *RecoveryCheckpoint) bool {
	return reflect.DeepEqual(left, right)
}

func (s *Scanner) importTakeoverCheckpoint(ctx context.Context, current *State) error {
	saved := current.Takeover.Checkpoint
	if saved == nil || current.Checkpoint != nil {
		return nil
	}
	cursor := saved.Cursor
	if cursor != "" && current.Spec.Source.Kind == "LOCAL" && current.Spec.InventoryID == "" {
		store, err := s.factory.OpenEndpoint(ctx, current.Spec.Source)
		if err != nil {
			return err
		}
		importer, ok := store.(interface {
			ImportScanCursor(transferstore.ListOptions, string) (string, error)
		})
		if !ok {
			_ = store.Close()
			return errors.New("source cannot import its shared directory scan checkpoint")
		}
		cursor, err = importer.ImportScanCursor(transferstore.ListOptions{Prefix: current.Spec.Prefix, StartAfter: current.Spec.StartKey, Cursor: cursor}, saved.SourceRoot)
		closeErr := store.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	// Seeded assignments are already counted as seen, including those behind
	// the saved cursor. A replay of the partially assigned page checks identity
	// but does not count these objects again. Keep a distinct pass so new local
	// objects retain strict same-mount identity checks on ordinary restarts.
	current.Checkpoint = &Checkpoint{Version: 1, Pass: fmt.Sprintf("takeover-resume:%d", current.Epoch), Cursor: cursor, SourceDone: saved.SourceDone}
	current.SeenObjects = current.TotalObjects
	copy := *saved
	copy.Cursor, copy.SourceRoot, copy.Epoch = cursor, current.Spec.Source.Root, current.Epoch
	current.TransferCheckpoint = &copy
	return nil
}
