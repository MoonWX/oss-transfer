package task

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func childStorageFixture(t *testing.T) Child {
	t.Helper()
	when := time.Date(2026, 10, 23, 10, 20, 30, 123456789, time.UTC)
	job := execution.Job{
		ChildID: "224-7", ParentID: 224, Number: 7, Generation: 3,
		Source: execution.Endpoint{Kind: "S3", Ref: "source", Bucket: "source-bucket"}, Destination: execution.Endpoint{Kind: "S3", Ref: "target", Bucket: "target-bucket"},
		ConflictPolicy: "OVERWRITE", DiscoveryComplete: true,
		RequestLimits: &execution.RequestLimits{ListQPS: 8, HeadQPS: 100, PutQPS: 50, Scope: "CLUSTER"},
		Items: []execution.Item{{ID: "1", Source: transferstore.Object{
			Key: "目录/with space.bin", Size: 4096, ModifiedAt: when, ETag: "opaque-etag", VersionID: "source-version",
			Metadata: map[string]string{"retained": "source metadata"}, ExpiresAt: when.Add(time.Hour), LifecycleExpiresAt: when.Add(24 * time.Hour),
		}, DestinationKey: "target/目录/with space.bin", Legacy: &execution.LegacyCandidate{Successful: true, SourceChanged: true}}},
	}
	// Independently allocated snapshots, as read from the original JSON format.
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	var recordJob execution.Job
	if err := json.Unmarshal(raw, &recordJob); err != nil {
		t.Fatal(err)
	}
	record := &execution.Record{
		Job: recordJob, Status: execution.Completed, Attempt: 2, Fingerprint: "persisted-immutable-fingerprint", CreatedAt: when, UpdatedAt: when.Add(time.Second),
		Counters: execution.Counters{Total: 1, TotalBytes: 4096, Succeeded: 1, SucceededBytes: 4096},
		Results:  []execution.ItemResult{{ID: "1", Status: execution.ItemSucceeded, Target: transferstore.Object{Key: job.Items[0].DestinationKey, Size: 4096, ETag: "target-etag", VersionID: "target-version", ModifiedAt: when}, TargetCommitted: true}},
	}
	return Child{Job: job, Record: record, Accepted: true, Node: Node{ID: "node-a", EnrollmentID: "enrollment-a", Address: "worker-a"}}
}

func encodeChild(t *testing.T, c Child, compact bool) []byte {
	t.Helper()
	var value any = c
	if compact {
		value = state.ForStorage(c)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestChildAssignmentReferencePreservesOldAPIAndFingerprint(t *testing.T) {
	c := childStorageFixture(t)
	api := encodeChild(t, c, false)
	stored := encodeChild(t, c, true)
	if !bytes.Contains(stored, []byte(`"recordJobRef":true`)) || bytes.Count(stored, []byte(`"job":`)) != 1 {
		t.Fatalf("equal assignment was not stored once: %s", stored)
	}
	if bytes.Contains(api, []byte("recordJobRef")) || bytes.Count(api, []byte(`"job":`)) != 2 {
		t.Fatal("storage optimization changed public JSON")
	}
	for name, raw := range map[string][]byte{"old JSON": api, "reference format": stored} {
		t.Run(name, func(t *testing.T) {
			var decoded Child
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(c, decoded) || !bytes.Equal(api, encodeChild(t, decoded, false)) {
				t.Fatal("storage round trip changed child fields or public JSON")
			}
			before, _ := json.Marshal(c.Job)
			after, _ := json.Marshal(decoded.Record.Job)
			if sha256.Sum256(before) != sha256.Sum256(after) || decoded.Record.Fingerprint != c.Record.Fingerprint {
				t.Fatal("restored assignment changed report digest or execution fingerprint")
			}
		})
	}
}

func TestChildAssignmentReferenceRestoresIndependentMutableSnapshots(t *testing.T) {
	c := childStorageFixture(t)
	var decoded Child
	if err := json.Unmarshal(encodeChild(t, c, true), &decoded); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(decoded.Job)
	decoded.Record.Job.Items[0].DestinationKey = "changed"
	decoded.Record.Job.Items[0].Source.Metadata["retained"] = "changed"
	decoded.Record.Job.Items[0].Legacy.SourceChanged = false
	decoded.Record.Job.RequestLimits.HeadQPS = 1
	after, _ := json.Marshal(decoded.Job)
	if !bytes.Equal(before, after) {
		t.Fatal("record mutation changed immutable master assignment through aliased data")
	}
	decoded.Job.Items[0].Source.Metadata["master-only"] = "new"
	if _, shared := decoded.Record.Job.Items[0].Source.Metadata["master-only"]; shared {
		t.Fatal("master mutation changed Agent snapshot through an aliased map")
	}
}

func TestChildAssignmentReferenceNeverMergesDifferentOwnershipOrLimits(t *testing.T) {
	for name, mutate := range map[string]func(*execution.Job){
		"generation":         func(j *execution.Job) { j.Generation++ },
		"discovery seal":     func(j *execution.Job) { j.DiscoveryComplete = false },
		"request QPS":        func(j *execution.Job) { j.RequestLimits.HeadQPS++ },
		"request scope":      func(j *execution.Job) { j.RequestLimits.Scope = "PER_NODE" },
		"request limits nil": func(j *execution.Job) { j.RequestLimits = nil },
		"destination":        func(j *execution.Job) { j.Items[0].DestinationKey = "other-key" },
		"source version":     func(j *execution.Job) { j.Items[0].Source.VersionID = "different-version" },
	} {
		t.Run(name, func(t *testing.T) {
			c := childStorageFixture(t)
			mutate(&c.Record.Job)
			stored := encodeChild(t, c, true)
			if bytes.Contains(stored, []byte("recordJobRef")) || bytes.Count(stored, []byte(`"job":`)) != 2 {
				t.Fatal("different assignments were collapsed")
			}
			var decoded Child
			if err := json.Unmarshal(stored, &decoded); err != nil || !reflect.DeepEqual(c, decoded) {
				t.Fatalf("distinct assignments changed on read: %v", err)
			}
		})
	}
}

func TestChildAssignmentReferenceRejectsAmbiguousOrIncompleteMarker(t *testing.T) {
	for name, raw := range map[string]string{
		"missing record":       `{"job":{"childId":"1-1"},"recordJobRef":true}`,
		"null record":          `{"job":{"childId":"1-1"},"record":null,"recordJobRef":true}`,
		"missing assignment":   `{"record":{},"recordJobRef":true}`,
		"two assignments":      `{"job":{"childId":"1-1"},"record":{"job":{"childId":"2-1"}},"recordJobRef":true}`,
		"partial embedded job": `{"job":{"childId":"1-1"},"record":{"job":{"generation":99}},"recordJobRef":true}`,
		"empty embedded job":   `{"job":{"childId":"1-1"},"record":{"job":{}},"recordJobRef":true}`,
		"null embedded job":    `{"job":{"childId":"1-1"},"record":{"job":null},"recordJobRef":true}`,
		"wrong marker type":    `{"job":{"childId":"1-1"},"record":{},"recordJobRef":"yes"}`,
	} {
		t.Run(name, func(t *testing.T) {
			c := childStorageFixture(t)
			before := encodeChild(t, c, false)
			if err := json.Unmarshal([]byte(raw), &c); err == nil {
				t.Fatal("invalid assignment reference was accepted")
			}
			if after := encodeChild(t, c, false); !bytes.Equal(before, after) {
				t.Fatal("rejected reference partially changed the previous child")
			}
		})
	}
}

func TestChildStoragePreservesZeroAndMissingRecordValues(t *testing.T) {
	for _, original := range []Child{{}, {Record: &execution.Record{}}, {Job: execution.Job{ChildID: "1-1"}}, {Job: execution.Job{ChildID: "1-1"}, Record: &execution.Record{}}} {
		stored := encodeChild(t, original, true)
		var decoded Child
		if err := json.Unmarshal(stored, &decoded); err != nil || !reflect.DeepEqual(original, decoded) {
			t.Fatalf("zero or missing snapshot failed round trip: %s %v", stored, err)
		}
	}
}
