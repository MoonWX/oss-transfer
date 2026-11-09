// Package comparison performs read-only, bounded-memory comparisons of object
// ranges. It never stages object contents on disk.
package comparison

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"maps"
	"strings"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

type ReaderWrapper func(context.Context, io.ReadCloser, int64) (io.ReadCloser, error)

func Validate(r Request) error {
	if r.ID == "" || len(r.ID) > 128 || strings.ContainsAny(r.ID, "/\x00") ||
		(r.Mode != "metadata" && r.Mode != "content") || (r.Scope != "prefix" && r.Scope != "object") ||
		len(r.SourcePrefix) > 1024 || len(r.DestinationPrefix) > 1024 || strings.ContainsRune(r.SourcePrefix+r.DestinationPrefix, '\x00') ||
		r.Source.Kind != "S3" || r.Destination.Kind != "S3" ||
		(r.Scope == "object" && (r.SourcePrefix == "" || r.DestinationPrefix == "")) {
		return errors.New("invalid comparison request")
	}
	return nil
}

type iterator struct {
	store                transferstore.Store
	prefix, cursor, last string
	// startAfter is a full key; listing excludes keys up to and including it.
	startAfter    string
	page          []transferstore.Object
	started, done bool
}

func (i *iterator) next(ctx context.Context) (*transferstore.Object, error) {
	for len(i.page) == 0 {
		if i.done {
			return nil, nil
		}
		p, err := i.store.List(ctx, transferstore.ListOptions{Prefix: i.prefix, StartAfter: i.startAfter, Cursor: i.cursor, Limit: 256})
		if err != nil {
			return nil, err
		}
		if p.NextCursor != "" && p.NextCursor == i.cursor {
			return nil, errors.New("object listing did not advance")
		}
		i.cursor, i.page, i.done = p.NextCursor, p.Objects, p.NextCursor == ""
	}
	o := i.page[0]
	i.page = i.page[1:]
	if !strings.HasPrefix(o.Key, i.prefix) || i.started && o.Key <= i.last || i.startAfter != "" && o.Key <= i.startAfter {
		return nil, errors.New("storage returned an unordered or out-of-scope object listing")
	}
	i.last, i.started = o.Key, true
	return &o, nil
}

// RunOptions tunes Run. The zero value compares from the first key.
type RunOptions struct {
	// StartAfter is a relative key: prefix comparisons list both sides after
	// it. Object scope ignores it.
	StartAfter string
	// Workers bounds concurrent object comparisons; 0 selects the default for
	// the mode.
	Workers int
	// OnBytes receives object content bytes as content mode reads them. It
	// may be called concurrently.
	OnBytes func(int64)
}

// Run merges two ordered listings. Removing the selected prefix on each side
// defines the relative key; no path cleaning or slash normalization is applied.
// emit is called from one goroutine, in ascending relative key order, so once
// emit returns for a row every key up to and including row.Key is counted.
func Run(ctx context.Context, r Request, source, dest transferstore.Store, wrap ReaderWrapper, options RunOptions, emit func(Row) error) error {
	if err := Validate(r); err != nil {
		return err
	}
	if r.Scope == "object" {
		a, ae := source.Stat(ctx, r.SourcePrefix)
		b, be := dest.Stat(ctx, r.DestinationPrefix)
		row := Row{Key: r.SourcePrefix, SourceKey: r.SourcePrefix, DestinationKey: r.DestinationPrefix}
		if ae != nil && !errors.Is(ae, transferstore.ErrNotFound) || be != nil && !errors.Is(be, transferstore.ErrNotFound) {
			row.Status = "ERROR"
			row.Error = "Cannot read object metadata"
		} else if ae != nil && be != nil {
			row.Status = "BOTH_MISSING"
		} else if ae != nil {
			row.Status = "EXTRA_TARGET"
			row.DestinationSize = b.Size
		} else if be != nil {
			row.Status = "MISSING_TARGET"
			row.SourceSize = a.Size
		} else {
			row = compare(ctx, r, source, dest, a, b, wrap)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return emit(row)
	}
	a, b := iterator{store: source, prefix: r.SourcePrefix}, iterator{store: dest, prefix: r.DestinationPrefix}
	if options.StartAfter != "" {
		a.startAfter, b.startAfter = r.SourcePrefix+options.StartAfter, r.DestinationPrefix+options.StartAfter
	}
	x, err := a.next(ctx)
	if err != nil {
		return err
	}
	y, err := b.next(ctx)
	if err != nil {
		return err
	}
	for x != nil || y != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
		var row Row
		xkey, ykey := "", ""
		if x != nil {
			xkey = strings.TrimPrefix(x.Key, r.SourcePrefix)
		}
		if y != nil {
			ykey = strings.TrimPrefix(y.Key, r.DestinationPrefix)
		}
		switch {
		case y == nil || x != nil && xkey < ykey:
			row = Row{Key: xkey, SourceKey: x.Key, DestinationKey: r.DestinationPrefix + xkey, Status: "MISSING_TARGET", SourceSize: x.Size}
			// HEAD confirms absence at observation time, rather than trusting two
			// independently changing LIST pages.
			if actual, e := dest.Stat(ctx, row.DestinationKey); e == nil {
				row = compare(ctx, r, source, dest, *x, actual, wrap)
			} else if !errors.Is(e, transferstore.ErrNotFound) {
				row.Status = "ERROR"
				row.Error = "Cannot read target metadata"
			} else if current, e := source.Stat(ctx, x.Key); errors.Is(e, transferstore.ErrNotFound) || e == nil && !sameVersion(*x, current) {
				row.Status = "CHANGED_DURING_COMPARE"
			} else if e != nil {
				row.Status = "ERROR"
				row.Error = "Cannot read source metadata"
			}
			x, err = a.next(ctx)
		case x == nil || ykey < xkey:
			row = Row{Key: ykey, SourceKey: r.SourcePrefix + ykey, DestinationKey: y.Key, Status: "EXTRA_TARGET", DestinationSize: y.Size}
			if actual, e := source.Stat(ctx, row.SourceKey); e == nil {
				row = compare(ctx, r, source, dest, actual, *y, wrap)
			} else if !errors.Is(e, transferstore.ErrNotFound) {
				row.Status = "ERROR"
				row.Error = "Cannot read source metadata"
			} else if current, e := dest.Stat(ctx, y.Key); errors.Is(e, transferstore.ErrNotFound) || e == nil && !sameVersion(*y, current) {
				row.Status = "CHANGED_DURING_COMPARE"
			} else if e != nil {
				row.Status = "ERROR"
				row.Error = "Cannot read target metadata"
			}
			y, err = b.next(ctx)
		default:
			row = compare(ctx, r, source, dest, *x, *y, wrap)
			x, err = a.next(ctx)
			if err == nil {
				y, err = b.next(ctx)
			}
		}
		if e := ctx.Err(); e != nil {
			return e
		}
		if e := emit(row); e != nil {
			return e
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func sameVersion(a, b transferstore.Object) bool {
	return a.Size == b.Size && (a.ETag == "" || transferstore.EqualETag(a.ETag, b.ETag)) &&
		(a.VersionID == "" || a.VersionID == b.VersionID) && (a.ModifiedAt.IsZero() || a.ModifiedAt.Equal(b.ModifiedAt))
}

func compare(ctx context.Context, r Request, source, dest transferstore.Store, listedA, listedB transferstore.Object, wrap ReaderWrapper) Row {
	row := Row{Key: strings.TrimPrefix(listedA.Key, r.SourcePrefix), SourceKey: listedA.Key, DestinationKey: listedB.Key, SourceSize: listedA.Size, DestinationSize: listedB.Size}
	if r.Scope == "object" {
		row.Key = listedA.Key
	}
	a, ae := source.Stat(ctx, listedA.Key)
	b, be := dest.Stat(ctx, listedB.Key)
	if ae != nil || be != nil {
		row.Status = "ERROR"
		row.Error = "Cannot read object metadata"
		if errors.Is(ae, transferstore.ErrNotFound) || errors.Is(be, transferstore.ErrNotFound) {
			row.Status = "CHANGED_DURING_COMPARE"
			row.Error = ""
		}
		return row
	}
	if !sameVersion(listedA, a) || !sameVersion(listedB, b) {
		row.Status = "CHANGED_DURING_COMPARE"
		return row
	}
	row.SourceSize, row.DestinationSize = a.Size, b.Size
	row.Status = "MATCH"
	if a.Size != b.Size {
		row.Status = "DIFFERENT"
		return row
	}
	if r.Mode == "metadata" {
		// Cross-provider ETag, modification dates and storage class are not
		// content equality. Compare the fields that transfers preserve.
		if a.ContentType != b.ContentType || a.ContentEncoding != b.ContentEncoding || a.ContentDisposition != b.ContentDisposition || a.CacheControl != b.CacheControl || !maps.Equal(a.Metadata, b.Metadata) {
			row.Status = "DIFFERENT"
		}
		return row
	}
	row.SourceSHA256, ae = hash(ctx, source, a, wrap)
	if ae == nil {
		row.DestinationSHA256, be = hash(ctx, dest, b, wrap)
	}
	if ae != nil || be != nil {
		row.Status = "ERROR"
		row.Error = "Cannot read complete object content"
		if errors.Is(ae, transferstore.ErrSourceChanged) || errors.Is(be, transferstore.ErrSourceChanged) {
			row.Status = "CHANGED_DURING_COMPARE"
			row.Error = ""
		}
		return row
	}
	// Both objects may have changed while the opposite side was streaming.
	afterA, ae := source.Stat(ctx, a.Key)
	afterB, be := dest.Stat(ctx, b.Key)
	if ae != nil && !errors.Is(ae, transferstore.ErrNotFound) || be != nil && !errors.Is(be, transferstore.ErrNotFound) {
		row.Status = "ERROR"
		row.Error = "Cannot read object metadata"
		return row
	}
	if ae != nil || be != nil || !sameVersion(a, afterA) || !sameVersion(b, afterB) {
		row.Status = "CHANGED_DURING_COMPARE"
		return row
	}
	if row.SourceSHA256 != row.DestinationSHA256 {
		row.Status = "DIFFERENT"
	}
	return row
}

func hash(ctx context.Context, store transferstore.Store, expected transferstore.Object, wrap ReaderWrapper) (string, error) {
	r, _, err := store.Open(ctx, expected.Key, transferstore.OpenOptions{Expected: &expected})
	if err != nil {
		return "", err
	}
	if wrap != nil {
		wrapped, e := wrap(ctx, r, expected.Size)
		if e != nil {
			_ = r.Close()
			return "", e
		}
		r = wrapped
	}
	defer r.Close()
	h := sha256.New()
	n, err := io.CopyBuffer(h, r, make([]byte, 64<<10))
	if err != nil {
		return "", err
	}
	if n != expected.Size {
		return "", transferstore.ErrSizeMismatch
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
