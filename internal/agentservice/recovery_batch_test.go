package agentservice

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func readyUploadRegistry(t *testing.T) (*S3RecoveryRegistry, *LocalTempRegistry, *state.Store) {
	t.Helper()
	temporary, db := openTempRegistry(t, filepath.Join(t.TempDir(), "agent.db"))
	if _, err := temporary.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	registry, err := NewS3RecoveryRegistry(db, &storageconfig.Factory{Source: storageconfig.NewAgent(db)}, temporary)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	return registry, temporary, db
}

func uploadRecord(registry *S3RecoveryRegistry, number int) transferstore.S3UploadRecord {
	return transferstore.S3UploadRecord{
		Owner: registry.OwnerID(), ID: fmt.Sprintf("%032x", number), Bucket: "bucket", Key: fmt.Sprintf("object-%d", number), UploadID: fmt.Sprintf("upload-%d", number),
		Connection: transferstore.S3ConnectionIdentity{Alias: "test", Endpoint: "http://127.0.0.1:1", Region: "test", SignatureVersion: "v4", CredentialFingerprint: strings.Repeat("a", 64)},
	}
}

// Hold the existing queue's writer while callers accumulate. This establishes
// the before-commit point without depending on disk speed or scheduler timing.
func holdRecoveryWriter(t *testing.T, temporary *LocalTempRegistry) func() {
	t.Helper()
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		finished <- temporary.submit(context.Background(), func(*state.Tx) error {
			close(entered)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("recovery writer did not start")
	}
	var once sync.Once
	unblock := func() {
		once.Do(func() {
			close(release)
			if err := <-finished; err != nil {
				t.Errorf("writer barrier: %v", err)
			}
		})
	}
	t.Cleanup(unblock)
	return unblock
}

func waitRecoveryQueue(t *testing.T, temporary *LocalTempRegistry, count int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(temporary.queue) != count {
		if time.Now().After(deadline) {
			t.Fatalf("expected %d queued recovery records, got %d", count, len(temporary.queue))
		}
		time.Sleep(time.Millisecond)
	}
}

func TestMultipartRegistrationCancellationStillWaitsForDurableCommit(t *testing.T) {
	registry, temporary, db := readyUploadRegistry(t)
	release := holdRecoveryWriter(t, temporary)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	record := uploadRecord(registry, 1)
	go func() { done <- registry.RegisterUpload(ctx, record) }()
	waitRecoveryQueue(t, temporary, 1)
	cancel()
	select {
	case err := <-done:
		t.Fatalf("registration returned before its queued write settled: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	records := uploadRecords(t, db)
	if len(records) != 1 || records[0] != record {
		t.Fatalf("successful registration did not persist its recovery identity: %+v", records)
	}
	if err := registry.ForgetUpload(context.Background(), record.ID); err != nil {
		t.Fatal(err)
	}
	if records := uploadRecords(t, db); len(records) != 0 {
		t.Fatalf("successful completion left upload recovery records: %+v", records)
	}
}

func TestMultipartRecoveryBatchFailureAcknowledgesNoUploads(t *testing.T) {
	registry, temporary, db := readyUploadRegistry(t)
	release := holdRecoveryWriter(t, temporary)
	const count = 32
	results := make(chan error, count)
	for i := 1; i <= count; i++ {
		record := uploadRecord(registry, i)
		go func() { results <- registry.RegisterUpload(context.Background(), record) }()
	}
	waitRecoveryQueue(t, temporary, count)
	injected := errors.New("injected recovery commit failure")
	failed := make(chan error, 1)
	go func() {
		failed <- temporary.submit(context.Background(), func(*state.Tx) error { return injected })
	}()
	waitRecoveryQueue(t, temporary, count+1)
	release()
	if err := <-failed; !errors.Is(err, injected) {
		t.Fatalf("injection did not fail the batch: %v", err)
	}
	for i := 0; i < count; i++ {
		if err := <-results; !errors.Is(err, injected) {
			t.Fatalf("upload acknowledged an uncommitted batch: %v", err)
		}
	}
	if records := uploadRecords(t, db); len(records) != 0 {
		t.Fatalf("failed atomic recovery batch left %d records", len(records))
	}
	for i := 1; i <= count; i++ {
		record := uploadRecord(registry, i)
		go func() { results <- registry.RegisterUpload(context.Background(), record) }()
	}
	for i := 0; i < count; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if records := uploadRecords(t, db); len(records) != count {
		t.Fatalf("retry must durably retain all %d upload identities, got %d", count, len(records))
	}
}
