package comparison

import (
	"context"
	"errors"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// unorderedStore lists its keys in the order given, ignoring sort order.
type unorderedStore struct {
	*fakeStore
	keys []string
}

func (s *unorderedStore) List(ctx context.Context, o transferstore.ListOptions) (transferstore.Page, error) {
	if err := ctx.Err(); err != nil {
		return transferstore.Page{}, err
	}
	page := transferstore.Page{}
	if o.Cursor != "" || o.StartAfter != "" {
		return page, nil
	}
	for _, key := range s.keys {
		page.Objects = append(page.Objects, object(key, 1))
	}
	return page, nil
}

// A storage that lists keys out of byte order cannot be compared. The failure
// says so instead of asking to check storage access, and the comparison is not
// offered for resuming: a resume lists the same storage again.
func TestUnusableListingFailsWithItsOwnMessageAndNoResume(t *testing.T) {
	h := newHarness(t)
	source := &unorderedStore{fakeStore: newFakeStore(object("src/a", 1), object("src/b", 1), object("src/c", 1)), keys: []string{"src/a", "src/c", "src/b"}}
	target := newFakeStore(object("backup/a", 1), object("backup/b", 1), object("backup/c", 1))
	m := h.open(storesFactory(source, target))
	p := startAndWait(t, m, requestWithID("unordered"))
	if p.Status != StateFailed || p.Error != errorListing || p.Resumable || p.ErrorDetail != `source listing is not in ascending byte order after key "c"` {
		t.Fatalf("unusable listing: %+v", p)
	}
	if _, err := m.Resume("unordered", nil); !errors.Is(err, ErrNotResumable) {
		t.Fatalf("a comparison of an unusable listing was resumed: %v", err)
	}
	// Any other failure keeps the general message and stays resumable.
	denied := newFakeStore(object("src/a", 1))
	denied.listHook = func(context.Context, transferstore.ListOptions) error { return accessDenied() }
	other := newHarness(t).open(storesFactory(denied, newFakeStore()))
	if p := startAndWait(t, other, requestWithID("denied")); p.Status != StateFailed || p.Error != errorFailed || !p.Resumable {
		t.Fatalf("access failure: %+v", p)
	}
}
