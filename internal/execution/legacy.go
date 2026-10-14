package execution

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"math"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// LegacyCandidate is an observation from an older execution ledger. It never
// grants success by itself, and its size or ETag is not a content checksum.
type LegacyCandidate struct {
	Successful    bool `json:"successful"`
	Missing       bool `json:"missing,omitempty"`
	SourceChanged bool `json:"sourceChanged,omitempty"`
}

func identifiable(o transferstore.Object) bool {
	return o.ETag != "" || o.VersionID != "" || !o.ModifiedAt.IsZero()
}

// verifyLegacy reads both complete objects, then checks their identities again.
// This is a current equality check, not evidence of an atomic source snapshot.
func (e *Engine) verifyLegacy(ctx context.Context, job Job, item Item, source, target transferstore.Store, src transferstore.Object) (bool, transferstore.Object, string, error) {
	dst, err := target.Stat(ctx, item.DestinationKey)
	if errors.Is(err, transferstore.ErrNotFound) {
		return false, dst, "TARGET_MISSING", nil
	}
	if err != nil {
		return false, dst, "TARGET_CHECK_FAILED", err
	}
	if !identifiable(src) || !identifiable(dst) {
		return false, dst, "IDENTITY_UNAVAILABLE", nil
	}
	// Even equal sizes and ETags cannot replace reading the object bodies.
	sourceHash, err := e.legacyHash(ctx, job, item, source, src)
	if err != nil {
		return false, dst, "SOURCE_VERIFY_FAILED", err
	}
	targetHash, err := e.legacyHash(ctx, job, item, target, dst)
	if err != nil {
		return false, dst, "TARGET_VERIFY_FAILED", err
	}
	afterSource, err := source.Stat(ctx, src.Key)
	if err != nil {
		return false, dst, "SOURCE_CHANGED", err
	}
	afterTarget, err := target.Stat(ctx, dst.Key)
	if err != nil {
		return false, dst, "TARGET_CHANGED", err
	}
	if changed(afterSource, src) || changed(afterTarget, dst) {
		return false, dst, "CHANGED_DURING_VERIFICATION", transferstore.ErrSourceChanged
	}
	if src.Size != dst.Size || sourceHash != targetHash {
		return false, dst, "CONTENT_MISMATCH", nil
	}
	reason := "SHA256_VERIFIED"
	if item.Legacy.SourceChanged {
		reason = "SOURCE_CHANGED_SHA256_VERIFIED"
	}
	return true, dst, reason, nil
}

func (e *Engine) legacyHash(ctx context.Context, job Job, item Item, store transferstore.Store, expected transferstore.Object) ([sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	if expected.Size < 0 || expected.Size == math.MaxInt64 {
		return digest, transferstore.ErrSizeMismatch
	}
	in, actual, err := store.Open(ctx, expected.Key, transferstore.OpenOptions{Expected: &expected})
	if err != nil {
		return digest, err
	}
	defer in.Close()
	closeOnCancel := context.AfterFunc(ctx, func() { _ = in.Close() })
	defer closeOnCancel()
	if changed(actual, expected) {
		return digest, transferstore.ErrSourceChanged
	}
	var reader io.ReadCloser = in
	if e.opts.DecorateReader != nil {
		reader, err = e.opts.DecorateReader(ctx, job, item, in)
		if err != nil {
			return digest, err
		}
		if reader == nil {
			return digest, errors.New("nil decorated verification reader")
		}
		defer reader.Close()
	}
	hash := sha256.New()
	// Copy reads through EOF, including the local store's EOF identity check.
	n, err := io.CopyBuffer(hash, io.LimitReader(reader, expected.Size+1), make([]byte, 64<<10))
	if err != nil {
		return digest, err
	}
	if n != expected.Size {
		return digest, transferstore.ErrSizeMismatch
	}
	if err := ctx.Err(); err != nil {
		return digest, err
	}
	if err := reader.Close(); err != nil {
		return digest, err
	}
	copy(digest[:], hash.Sum(nil))
	return digest, nil
}
