package comparison

import (
	"fmt"
	"testing"
)

func TestMetadataDoesNotReadContentOrTreatETagAsHash(t *testing.T) {
	a, b := local(t), local(t)
	put(t, a, "src/a", "AAAA")
	put(t, b, "backup/a", "BBBB")
	a.etag = "multipart-3"
	b.etag = "opaque-encrypted"
	rows := collect(t, request(), a, b)
	if len(rows) != 1 || rows[0].Status != "MATCH" || a.opens.Load() != 0 || b.opens.Load() != 0 {
		t.Fatalf("metadata claimed content equality or read bodies: %+v", rows)
	}
}

func TestStreamingDetectsSameSizeDifferenceAndBothSideMissing(t *testing.T) {
	a, b := local(t), local(t)
	put(t, a, "src/a", "same")
	put(t, b, "backup/a", "same")
	put(t, a, "src/b", "AAAA")
	put(t, b, "backup/b", "BBBB")
	put(t, a, "src/missing", "data")
	put(t, b, "backup/extra", "data")
	r := request()
	r.Mode = "content"
	rows := collect(t, r, a, b)
	want := map[string]string{"a": "MATCH", "b": "DIFFERENT", "missing": "MISSING_TARGET", "extra": "EXTRA_TARGET"}
	for _, row := range rows {
		if want[row.Key] != row.Status {
			t.Fatalf("wrong result: %+v", row)
		}
		delete(want, row.Key)
		if row.Key == "b" && (row.SourceSHA256 == row.DestinationSHA256 || len(row.SourceSHA256) != 64) {
			t.Fatalf("missing real SHA-256: %+v", row)
		}
	}
	if len(want) != 0 || a.opens.Load() != 2 || b.opens.Load() != 2 {
		t.Fatalf("missing results or unexpected reads: %v %d %d", want, a.opens.Load(), b.opens.Load())
	}
}

func TestMergePaginatesBothSidesAndPreservesExactKeyMapping(t *testing.T) {
	a, b := local(t), local(t)
	for i := 0; i < 520; i++ {
		k := fmt.Sprintf("%04d", i)
		put(t, a, "src/"+k, "x")
		put(t, b, "backup/"+k, "x")
	}
	put(t, b, "backup/zz extra + 字", "x")
	rows := collect(t, request(), a, b)
	if len(rows) != 521 || rows[520].Status != "EXTRA_TARGET" || rows[520].SourceKey != "src/zz extra + 字" {
		t.Fatalf("lost paged data: %d %+v", len(rows), rows[len(rows)-1])
	}
}

func TestContentChangedWhileOtherSideStreamsIsNotAMatch(t *testing.T) {
	a, b := local(t), local(t)
	put(t, a, "src/a", "AAAA")
	put(t, b, "backup/a", "AAAA")
	b.mutate = func() { put(t, a, "src/a", "BBBB") }
	r := request()
	r.Mode = "content"
	rows := collect(t, r, a, b)
	if rows[0].Status != "CHANGED_DURING_COMPARE" {
		t.Fatalf("changing object accepted: %+v", rows)
	}
}
