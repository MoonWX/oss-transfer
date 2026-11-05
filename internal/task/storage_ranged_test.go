package task

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// The Master keeps the task's ranged-download choice ("" or "off") while the
// Agent reports the delivered form ("auto", "off" or, from an Agent that
// predates it, ""). The assignment is still stored once, and each side's
// value comes back unchanged.
func TestChildAssignmentReferenceKeepsBothRangedDownloadValues(t *testing.T) {
	for _, pair := range [][2]string{{"", ""}, {"", "auto"}, {"off", "off"}, {"off", ""}, {"", "off"}} {
		c := childStorageFixture(t)
		c.Job.RangedDownload, c.Record.Job.RangedDownload = pair[0], pair[1]
		stored := encodeChild(t, c, true)
		if !bytes.Contains(stored, []byte(`"recordJobRef":true`)) || bytes.Count(stored, []byte(`"job":`)) != 1 {
			t.Fatalf("%q/%q: assignment stored twice: %s", pair[0], pair[1], stored)
		}
		if side := bytes.Contains(stored, []byte(`"recordRangedDownload"`)); side != (pair[0] != pair[1]) {
			t.Fatalf("%q/%q: side value present=%v", pair[0], pair[1], side)
		}
		var decoded Child
		if err := json.Unmarshal(stored, &decoded); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(c, decoded) || decoded.Job.RangedDownload != pair[0] || decoded.Record.Job.RangedDownload != pair[1] {
			t.Fatalf("%q/%q: round trip gave %q/%q", pair[0], pair[1], decoded.Job.RangedDownload, decoded.Record.Job.RangedDownload)
		}
	}
	var c Child
	if err := json.Unmarshal([]byte(`{"job":{"childId":"1-1"},"record":{"job":{"childId":"1-1"}},"recordRangedDownload":"auto"}`), &c); err == nil {
		t.Fatal("side value without a reference was accepted")
	}
}
