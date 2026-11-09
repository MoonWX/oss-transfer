// Package comparison performs read-only, bounded-memory comparisons of object
// ranges. It never stages object contents on disk.
package comparison

import (
	"context"
	"errors"
	"io"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

type ReaderWrapper func(context.Context, io.ReadCloser, int64) (io.ReadCloser, error)

// comparisonHeadTimeout bounds one HEAD, including the SDK's retries of it. A package
// variable so tests can shorten it.
var comparisonHeadTimeout = 2 * time.Minute

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
//
// One goroutine merges the listings, which are fetched a page ahead; workers
// compare the merged keys concurrently and their rows are reordered before
// emit. At most 4 × Workers keys are in flight or waiting to be emitted. The
// emitted rows are always a prefix of the rows a complete run emits: when Run
// fails or ctx ends, it emits the finished rows that are next in order, waits
// for its goroutines and returns. Rows computed after that point are dropped.
//
// A LIST failure is returned as "source LIST: ..." or "target LIST: ..." after
// retries; a listing that is not in ascending byte order, leaves the prefix or
// does not advance fails with an error naming the side; an emit error is
// returned as is; cancellation returns ctx.Err(). ErrorDetail describes any of
// them without storage response text.
func Run(ctx context.Context, r Request, source, dest transferstore.Store, wrap ReaderWrapper, options RunOptions, emit func(Row) error) error {
	if err := Validate(r); err != nil {
		return err
	}
	c := &comparer{request: r, source: source, target: dest, wrap: wrap, onBytes: options.OnBytes}
	if r.Scope == "object" {
		row := c.object(ctx)
		if err := ctx.Err(); err != nil {
			return err
		}
		return emit(row)
	}
	return c.prefix(ctx, options, emit)
}

// comparer holds one Run's request and stores.
type comparer struct {
	request        Request
	source, target transferstore.Store
	wrap           ReaderWrapper
	onBytes        func(int64)
}

// comparisonItem is one merged key: present on both sides or on one side.
type comparisonItem struct {
	seq            int64
	key            string
	source, target *transferstore.Object
}

type comparisonResult struct {
	seq int64
	row Row
}

func (c *comparer) prefix(parent context.Context, options RunOptions, emit func(Row) error) error {
	workers := options.Workers
	if workers <= 0 {
		workers = DefaultWorkers
		if c.request.Mode == "content" {
			workers = DefaultContentWorkers
		}
	}
	window := 4 * workers
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var failOnce sync.Once
	var failure error
	fail := func(err error) {
		failOnce.Do(func() {
			failure = err
			cancel()
		})
	}

	source := newComparisonListing("source", c.source, c.request.SourcePrefix, options.StartAfter)
	target := newComparisonListing("target", c.target, c.request.DestinationPrefix, options.StartAfter)
	var listers sync.WaitGroup
	listers.Add(2)
	go func() { defer listers.Done(); source.run(ctx) }()
	go func() { defer listers.Done(); target.run(ctx) }()

	// slots bounds keys dispatched but not yet emitted.
	slots := make(chan struct{}, window)
	items := make(chan comparisonItem)
	var dispatched int64
	var merged bool
	mergeDone := make(chan struct{})
	go func() {
		defer close(mergeDone)
		defer close(items)
		dispatched, merged = c.merge(ctx, source, target, slots, items, fail)
	}()

	// results never blocks: every result holds one of window slots.
	results := make(chan comparisonResult, window)
	var compared sync.WaitGroup
	compared.Add(workers)
	for range workers {
		go func() {
			defer compared.Done()
			for item := range items {
				if ctx.Err() != nil {
					continue
				}
				row := c.compareItem(ctx, item)
				if ctx.Err() != nil {
					// The row may reflect the cancellation; never emit it.
					continue
				}
				results <- comparisonResult{seq: item.seq, row: row}
			}
		}()
	}
	go func() {
		compared.Wait()
		close(results)
	}()

	pending := make([]Row, window)
	ready := make([]bool, window)
	var next int64
	var emitErr error
	for result := range results {
		index := result.seq % int64(window)
		pending[index], ready[index] = result.row, true
		for emitErr == nil {
			index = next % int64(window)
			if !ready[index] {
				break
			}
			row := pending[index]
			pending[index], ready[index] = Row{}, false
			if err := emit(row); err != nil {
				emitErr = err
				fail(err)
				break
			}
			next++
			<-slots
		}
	}
	<-mergeDone
	listers.Wait()
	compared.Wait()
	if merged && emitErr == nil && next == dispatched {
		return nil
	}
	if failure != nil {
		return failure
	}
	if err := parent.Err(); err != nil {
		return err
	}
	return errors.New("comparison stopped unexpectedly")
}

// merge walks both listings in one goroutine and dispatches every key in
// ascending order. It returns the number of dispatched keys and whether both
// listings were complete.
func (c *comparer) merge(ctx context.Context, source, target *comparisonListing, slots chan<- struct{}, items chan<- comparisonItem, fail func(error)) (int64, bool) {
	var count int64
	for {
		item, done, err := c.nextItem(ctx, source, target)
		if err != nil {
			if ctx.Err() == nil {
				fail(err)
			}
			return count, false
		}
		if done {
			return count, true
		}
		item.seq = count
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return count, false
		}
		select {
		case items <- item:
			count++
		case <-ctx.Done():
			return count, false
		}
	}
}

// nextItem takes the smallest relative key from the heads of both listings.
func (c *comparer) nextItem(ctx context.Context, source, target *comparisonListing) (comparisonItem, bool, error) {
	x, err := source.peek(ctx)
	if err != nil {
		return comparisonItem{}, false, err
	}
	y, err := target.peek(ctx)
	if err != nil {
		return comparisonItem{}, false, err
	}
	if x == nil && y == nil {
		return comparisonItem{}, true, nil
	}
	xkey, ykey := "", ""
	if x != nil {
		xkey = strings.TrimPrefix(x.Key, c.request.SourcePrefix)
	}
	if y != nil {
		ykey = strings.TrimPrefix(y.Key, c.request.DestinationPrefix)
	}
	switch {
	case y == nil || x != nil && xkey < ykey:
		source.pop()
		return comparisonItem{key: xkey, source: x}, false, nil
	case x == nil || ykey < xkey:
		target.pop()
		return comparisonItem{key: ykey, target: y}, false, nil
	default:
		source.pop()
		target.pop()
		return comparisonItem{key: xkey, source: x, target: y}, false, nil
	}
}

func (c *comparer) compareItem(ctx context.Context, item comparisonItem) Row {
	switch {
	case item.source != nil && item.target != nil:
		return c.compare(ctx, *item.source, *item.target, item.key)
	case item.source != nil:
		return c.missingTarget(ctx, *item.source, item.key)
	default:
		return c.extraTarget(ctx, *item.target, item.key)
	}
}

// missingTarget handles a key listed only on the source side. HEAD confirms
// absence at observation time, rather than trusting two independently
// changing LIST pages.
func (c *comparer) missingTarget(ctx context.Context, listed transferstore.Object, key string) Row {
	row := Row{Key: key, SourceKey: listed.Key, DestinationKey: c.request.DestinationPrefix + key, Status: StatusMissingTarget, SourceSize: listed.Size}
	actual, err := c.stat(ctx, c.target, row.DestinationKey)
	if err == nil {
		return c.compare(ctx, listed, actual, key)
	}
	if !objectNotFound(err) {
		return comparisonErrorRow(row, "Cannot read target metadata", comparisonSideDetail("HEAD", nil, err))
	}
	current, err := c.stat(ctx, c.source, listed.Key)
	switch {
	case objectNotFound(err) || err == nil && !sameVersion(listed, current):
		row.Status = StatusChanged
	case err != nil:
		return comparisonErrorRow(row, "Cannot read source metadata", comparisonSideDetail("HEAD", err, nil))
	}
	return row
}

// extraTarget handles a key listed only on the target side.
func (c *comparer) extraTarget(ctx context.Context, listed transferstore.Object, key string) Row {
	row := Row{Key: key, SourceKey: c.request.SourcePrefix + key, DestinationKey: listed.Key, Status: StatusExtraTarget, DestinationSize: listed.Size}
	actual, err := c.stat(ctx, c.source, row.SourceKey)
	if err == nil {
		return c.compare(ctx, actual, listed, key)
	}
	if !objectNotFound(err) {
		return comparisonErrorRow(row, "Cannot read source metadata", comparisonSideDetail("HEAD", err, nil))
	}
	current, err := c.stat(ctx, c.target, listed.Key)
	switch {
	case objectNotFound(err) || err == nil && !sameVersion(listed, current):
		row.Status = StatusChanged
	case err != nil:
		return comparisonErrorRow(row, "Cannot read target metadata", comparisonSideDetail("HEAD", nil, err))
	}
	return row
}

// object compares the single object named by both prefixes.
func (c *comparer) object(ctx context.Context) Row {
	r := c.request
	row := Row{Key: r.SourcePrefix, SourceKey: r.SourcePrefix, DestinationKey: r.DestinationPrefix}
	a, ae := c.stat(ctx, c.source, r.SourcePrefix)
	b, be := c.stat(ctx, c.target, r.DestinationPrefix)
	if sourceErr, targetErr := headFailure(ae), headFailure(be); sourceErr != nil || targetErr != nil {
		return comparisonErrorRow(row, "Cannot read object metadata", comparisonSideDetail("HEAD", sourceErr, targetErr))
	}
	switch {
	case ae != nil && be != nil:
		row.Status = StatusBothMissing
	case ae != nil:
		row.Status, row.DestinationSize = StatusExtraTarget, b.Size
	case be != nil:
		row.Status, row.SourceSize = StatusMissingTarget, a.Size
	default:
		return c.compare(ctx, a, b, r.SourcePrefix)
	}
	return row
}

// compare checks a key present on both sides. The listed objects come from
// LIST or HEAD; both sides are read again with HEAD before comparing.
func (c *comparer) compare(ctx context.Context, listedA, listedB transferstore.Object, key string) Row {
	row := Row{Key: key, SourceKey: listedA.Key, DestinationKey: listedB.Key, SourceSize: listedA.Size, DestinationSize: listedB.Size}
	a, ae := c.stat(ctx, c.source, listedA.Key)
	b, be := c.stat(ctx, c.target, listedB.Key)
	if ae != nil || be != nil {
		if objectNotFound(ae) || objectNotFound(be) {
			row.Status = StatusChanged
			return row
		}
		return comparisonErrorRow(row, "Cannot read object metadata", comparisonSideDetail("HEAD", ae, be))
	}
	if !sameVersion(listedA, a) || !sameVersion(listedB, b) {
		row.Status = StatusChanged
		return row
	}
	row.SourceSize, row.DestinationSize = a.Size, b.Size
	if c.request.Mode == "metadata" {
		// Cross-provider ETags, modification dates and storage classes are
		// not content equality. Compare the fields that transfers preserve.
		if reasons := metadataReasons(a, b); len(reasons) > 0 {
			row.Status, row.Reasons = StatusDifferent, reasons
		} else if md5ETagsDiffer(a.ETag, b.ETag) {
			row.Status, row.Reasons = StatusETagMismatch, []string{ReasonETag}
		} else {
			row.Status = StatusMatch
		}
		return row
	}
	if a.Size != b.Size {
		row.Status, row.Reasons = StatusDifferent, []string{ReasonSize}
		return row
	}
	sourceSum, targetSum, ae, be := c.hashBoth(ctx, a, b)
	if ae != nil || be != nil {
		if objectChanged(ae) || objectChanged(be) {
			row.Status = StatusChanged
			return row
		}
		return comparisonErrorRow(row, "Cannot read complete object content", comparisonSideDetail("GET", ae, be))
	}
	// Both objects may have changed while the opposite side was streaming.
	afterA, ae := c.stat(ctx, c.source, a.Key)
	afterB, be := c.stat(ctx, c.target, b.Key)
	if sourceErr, targetErr := headFailure(ae), headFailure(be); sourceErr != nil || targetErr != nil {
		return comparisonErrorRow(row, "Cannot read object metadata", comparisonSideDetail("HEAD", sourceErr, targetErr))
	}
	if ae != nil || be != nil || !sameVersion(a, afterA) || !sameVersion(b, afterB) {
		row.Status = StatusChanged
		return row
	}
	row.SourceSHA256, row.DestinationSHA256 = sourceSum, targetSum
	row.Status = StatusMatch
	if sourceSum != targetSum {
		row.Status, row.Reasons = StatusDifferent, []string{ReasonSHA256}
	}
	return row
}

// stat is a HEAD bounded by comparisonHeadTimeout.
func (c *comparer) stat(ctx context.Context, store transferstore.Store, key string) (transferstore.Object, error) {
	limit := comparisonHeadTimeout
	callCtx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	object, err := store.Stat(callCtx, key)
	if err != nil && ctx.Err() == nil && callCtx.Err() != nil && !objectChanged(err) {
		err = &comparisonTimeoutError{operation: "HEAD", limit: limit}
	}
	return object, err
}

// headFailure is err unless it only reports a missing object.
func headFailure(err error) error {
	if objectNotFound(err) {
		return nil
	}
	return err
}

func comparisonErrorRow(row Row, message, detail string) Row {
	row.Status, row.Error, row.ErrorDetail = StatusError, message, detail
	row.Reasons, row.SourceSHA256, row.DestinationSHA256 = nil, "", ""
	return row
}

// sameVersion reports whether b, observed later, is the version a describes.
// With ETags on both sides it compares size and ETag (and version IDs when
// both are known), never times: LIST can report milliseconds while HEAD has
// whole seconds. Only without ETags and version IDs does the modification
// time decide, truncated to whole seconds.
func sameVersion(a, b transferstore.Object) bool {
	if a.Size != b.Size {
		return false
	}
	versions := a.VersionID != "" && b.VersionID != ""
	if versions && a.VersionID != b.VersionID {
		return false
	}
	if a.ETag != "" && b.ETag != "" {
		return transferstore.EqualETag(a.ETag, b.ETag)
	}
	if versions || a.ModifiedAt.IsZero() {
		return true
	}
	return a.ModifiedAt.Truncate(time.Second).Equal(b.ModifiedAt.Truncate(time.Second))
}

// metadataReasons lists the compared properties that differ, in Reason order.
func metadataReasons(a, b transferstore.Object) []string {
	var reasons []string
	if a.Size != b.Size {
		reasons = append(reasons, ReasonSize)
	}
	if a.ContentType != b.ContentType {
		reasons = append(reasons, ReasonContentType)
	}
	if a.ContentEncoding != b.ContentEncoding {
		reasons = append(reasons, ReasonContentEncoding)
	}
	if a.ContentDisposition != b.ContentDisposition {
		reasons = append(reasons, ReasonContentDisposition)
	}
	if a.CacheControl != b.CacheControl {
		reasons = append(reasons, ReasonCacheControl)
	}
	if !maps.Equal(a.Metadata, b.Metadata) {
		reasons = append(reasons, ReasonMetadata)
	}
	return reasons
}

// md5ETagsDiffer reports two simple (single-part) ETags, each exactly 32 hex
// digits once quotes are removed, that differ. Multipart ETags ("...-N") and
// other forms are never compared: they are not content digests.
func md5ETagsDiffer(a, b string) bool {
	a, b = strings.Trim(a, `"`), strings.Trim(b, `"`)
	return md5ETag(a) && md5ETag(b) && !strings.EqualFold(a, b)
}

func md5ETag(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}
