package agentservice

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func abandonedRecords(t *testing.T, db *state.Store) []transferstore.S3UploadRecord {
	t.Helper()
	var records []transferstore.S3UploadRecord
	if err := db.View(func(tx *state.Tx) error {
		return tx.Scan(s3AbandonedBucket, "", "", 1000, func(_ string, raw json.RawMessage) error {
			var record transferstore.S3UploadRecord
			if err := json.Unmarshal(raw, &record); err != nil {
				return err
			}
			records = append(records, record)
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	return records
}

// abortServer denies aborts while the storage is "down" and accepts them
// afterwards, counting each abort request.
type abortServer struct {
	down   atomic.Bool
	aborts atomic.Int32
	server *httptest.Server
}

func newAbortServer(t *testing.T) *abortServer {
	s := &abortServer{}
	s.down.Store(true)
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		switch {
		case r.Method == "DELETE" && s.down.Load():
			s.aborts.Add(1)
			w.WriteHeader(403)
			fmt.Fprint(w, "<Error><Code>AccessDenied</Code><Message>unavailable</Message></Error>")
		case r.Method == "DELETE":
			s.aborts.Add(1)
			w.WriteHeader(204)
		default: // ListParts after a successful abort
			w.WriteHeader(404)
			fmt.Fprint(w, "<Error><Code>NoSuchUpload</Code><Message>gone</Message></Error>")
		}
	}))
	t.Cleanup(s.server.Close)
	return s
}

func abandonedFixture(t *testing.T, server *abortServer) (*S3RecoveryRegistry, *LocalTempRegistry, *storageconfig.Factory, *state.Store, transferstore.S3UploadRecord) {
	t.Helper()
	temporary, db := openTempRegistry(t, filepath.Join(t.TempDir(), "agent.db"))
	if _, err := temporary.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	pathStyle, v2 := true, false
	configs := storageconfig.NewAgent(db)
	if _, err := configs.ApplySnapshot(storageconfig.Snapshot{Revision: 1, Connections: []storageconfig.Config{{Alias: "saved", Endpoint: server.server.URL, Region: "us-east-1", AccessKey: "test-access", SecretKey: "fake-test-secret", PathStyleEnable: &pathStyle, UseV2SignerType: &v2}}}); err != nil {
		t.Fatal(err)
	}
	factory := &storageconfig.Factory{Source: configs, HTTPClient: server.server.Client(), RetryMaxAttempts: 1}
	registry, err := NewS3RecoveryRegistry(db, factory, temporary)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	client, err := factory.OpenEndpoint(context.Background(), execution.Endpoint{Kind: "S3", Ref: "saved", Bucket: "bucket"})
	if err != nil {
		t.Fatal(err)
	}
	record := transferstore.S3UploadRecord{Owner: registry.OwnerID(), ID: strings.Repeat("c", 32), Connection: client.(*transferstore.S3).ConnectionIdentity("saved"), Bucket: "bucket", Key: "object", UploadID: "orphaned-upload"}
	client.Close()
	if err := registry.RegisterUpload(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	return registry, temporary, factory, db, record
}

// An upload whose abort failed while the storage was down is aborted later,
// while the Agent runs: on a retry round and at the next start.
func TestAbandonedUploadsAreAbortedWhenStorageReturns(t *testing.T) {
	server := newAbortServer(t)
	registry, temporary, factory, db, record := abandonedFixture(t, server)
	if err := registry.AbandonUpload(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if live, abandoned := uploadRecords(t, db), abandonedRecords(t, db); len(live) != 0 || len(abandoned) != 1 || abandoned[0] != record {
		t.Fatalf("abandon did not move the record: %+v %+v", live, abandoned)
	}
	// Replaying the hand-over changes nothing.
	if err := registry.AbandonUpload(context.Background(), record); err != nil || len(abandonedRecords(t, db)) != 1 {
		t.Fatalf("replayed abandon: %v", err)
	}
	report, err := registry.RetryAbandoned(context.Background())
	if err != nil || report.Removed != 0 || len(report.Retained) != 1 || len(abandonedRecords(t, db)) != 1 || server.aborts.Load() != 1 {
		t.Fatalf("abort while the storage is down: %+v %v aborts=%d", report, err, server.aborts.Load())
	}
	// A restart in between still knows about it.
	restarted, err := NewS3RecoveryRegistry(db, factory, temporary)
	if err != nil {
		t.Fatal(err)
	}
	if report, err := restarted.Recover(context.Background()); err != nil || len(report.Retained) != 1 || len(abandonedRecords(t, db)) != 1 {
		t.Fatalf("restart lost or cleared the abandoned upload: %+v %v", report, err)
	}
	server.down.Store(false)
	report, err = restarted.RetryAbandoned(context.Background())
	if err != nil || report.Removed != 1 || len(report.Retained) != 0 || len(abandonedRecords(t, db)) != 0 || len(uploadRecords(t, db)) != 0 {
		t.Fatalf("abort after the storage returned: %+v %v", report, err)
	}
}

func runRegistry(t *testing.T, registry *S3RecoveryRegistry) {
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		registry.Run(ctx)
	}()
	t.Cleanup(func() { cancel(); <-stopped })
}

func TestAbandonedUploadRetriesRunWhenKicked(t *testing.T) {
	server := newAbortServer(t)
	registry, _, _, db, record := abandonedFixture(t, server)
	rounds := make(chan S3RecoveryReport)
	registry.rounds = rounds
	registry.retryGap, registry.retryEvery = 0, time.Hour
	runRegistry(t, registry)
	if err := registry.AbandonUpload(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	registry.Kick()
	if report := <-rounds; report.Removed != 0 || len(report.Retained) != 1 {
		t.Fatalf("round while the storage is down: %+v", report)
	}
	server.down.Store(false)
	registry.Kick()
	if report := <-rounds; report.Removed != 1 || len(abandonedRecords(t, db)) != 0 {
		t.Fatalf("round after the storage returned: %+v", report)
	}
}

func TestAbandonedUploadRetriesRunPeriodically(t *testing.T) {
	server := newAbortServer(t)
	server.down.Store(false)
	registry, _, _, db, record := abandonedFixture(t, server)
	rounds := make(chan S3RecoveryReport)
	registry.rounds = rounds
	registry.retryGap, registry.retryEvery = time.Millisecond, time.Millisecond
	if err := registry.AbandonUpload(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	runRegistry(t, registry)
	if report := <-rounds; report.Removed != 1 || len(abandonedRecords(t, db)) != 0 {
		t.Fatalf("periodic round: %+v", report)
	}
}
