package comparison

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// Listing limits. Package variables so tests can shorten them.
var (
	comparisonListPageSize = 1000
	// listAttemptTimeout bounds one LIST request, including the SDK's own
	// retries of it.
	listAttemptTimeout = 3 * time.Minute
	// listRetryDelays are the waits before each further LIST attempt after a
	// retryable failure; its length is the number of retries.
	listRetryDelays = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}
)

// comparisonPage is one LIST page passed from a lister to the merge. final
// marks the last page; a closed channel without a final page means the lister
// stopped because the comparison was canceled.
type comparisonPage struct {
	objects []transferstore.Object
	final   bool
	err     error
}

// comparisonListing lists one side ahead of the merge and checks that the
// keys it hands out are inside the prefix and strictly ascending.
type comparisonListing struct {
	side       string // "source" or "target"
	store      transferstore.Store
	prefix     string
	startAfter string // full key; "" lists from the start
	pages      chan comparisonPage

	// Merge goroutine state.
	page     []transferstore.Object
	head     *transferstore.Object
	last     string
	ordered  bool // last holds a key every later key must exceed
	finished bool
}

func newComparisonListing(side string, store transferstore.Store, prefix, relativeStartAfter string) *comparisonListing {
	l := &comparisonListing{side: side, store: store, prefix: prefix, pages: make(chan comparisonPage)}
	if relativeStartAfter != "" {
		l.startAfter = prefix + relativeStartAfter
		l.last, l.ordered = l.startAfter, true
	}
	return l
}

// run fetches pages in order. The channel is unbuffered, so the lister holds
// at most the one page after the page being merged.
func (l *comparisonListing) run(ctx context.Context) {
	defer close(l.pages)
	cursor := ""
	for {
		page, err := listComparisonPage(ctx, l.store, transferstore.ListOptions{Prefix: l.prefix, StartAfter: l.startAfter, Cursor: cursor, Limit: comparisonListPageSize})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			err = &comparisonSideError{side: l.side, operation: "LIST", err: err}
		} else if page.NextCursor != "" && page.NextCursor == cursor {
			err = &comparisonListingError{message: l.side + " listing did not advance"}
		}
		result := comparisonPage{objects: page.Objects, final: err == nil && page.NextCursor == "", err: err}
		if err != nil {
			result.objects = nil
		}
		select {
		case l.pages <- result:
		case <-ctx.Done():
			return
		}
		if err != nil || result.final {
			return
		}
		cursor = page.NextCursor
	}
}

// peek returns the next listed object without consuming it, or nil when the
// listing is complete.
func (l *comparisonListing) peek(ctx context.Context) (*transferstore.Object, error) {
	if l.head != nil {
		return l.head, nil
	}
	for len(l.page) == 0 {
		if l.finished {
			return nil, nil
		}
		select {
		case page, ok := <-l.pages:
			if !ok {
				// Only cancellation stops a lister before its final page.
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				return nil, &comparisonListingError{message: l.side + " listing stopped unexpectedly"}
			}
			if page.err != nil {
				return nil, page.err
			}
			l.page, l.finished = page.objects, page.final
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	object := l.page[0]
	l.page = l.page[1:]
	if !strings.HasPrefix(object.Key, l.prefix) {
		return nil, &comparisonListingError{message: l.side + " listing returned a key outside the prefix"}
	}
	if l.ordered && object.Key <= l.last {
		return nil, &comparisonListingError{message: fmt.Sprintf("%s listing is not in ascending byte order after key %q", l.side, clipDetail(strings.TrimPrefix(l.last, l.prefix), maxDetailKeyBytes))}
	}
	l.last, l.ordered = object.Key, true
	l.head = &object
	return l.head, nil
}

// pop consumes the object peek returned.
func (l *comparisonListing) pop() { l.head = nil }

// listComparisonPage lists one page, retrying retryable failures while ctx is
// live. Each attempt has its own time limit.
func listComparisonPage(ctx context.Context, store transferstore.Store, options transferstore.ListOptions) (transferstore.Page, error) {
	delays := listRetryDelays
	for attempt := 0; ; attempt++ {
		limit := listAttemptTimeout
		attemptCtx, cancel := context.WithTimeout(ctx, limit)
		page, err := store.List(attemptCtx, options)
		expired := attemptCtx.Err() != nil
		cancel()
		if err == nil {
			return page, nil
		}
		if ctx.Err() != nil {
			return transferstore.Page{}, ctx.Err()
		}
		if expired {
			// Only this attempt's deadline can end attemptCtx while ctx is live.
			err = &comparisonTimeoutError{operation: "LIST", limit: limit}
		}
		if attempt >= len(delays) || !retryableStorageError(err) {
			return transferstore.Page{}, err
		}
		if !sleepComparison(ctx, delays[attempt]) {
			return transferstore.Page{}, ctx.Err()
		}
	}
}

// sleepComparison waits for d unless ctx ends first.
func sleepComparison(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
