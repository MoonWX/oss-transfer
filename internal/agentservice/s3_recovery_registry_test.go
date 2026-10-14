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

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func uploadRecords(t *testing.T, db *state.Store) []transferstore.S3UploadRecord {
	t.Helper()
	var records []transferstore.S3UploadRecord
	if err := db.View(func(tx *state.Tx) error {
		return tx.Scan(s3UploadBucket, "", "", 1000, func(_ string, raw json.RawMessage) error {
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

func TestS3RecoveryUsesExactSavedConnectionAndRetainsChangedConfigurations(t *testing.T) {
	for _, change := range []string{"unchanged", "missing", "endpoint", "region", "signature", "pathStyle", "accessKey", "secretKey", "token", "owner", "no-such-upload", "denied"} {
		t.Run(change, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Query().Get("uploadId") != "owned-upload" || r.URL.Path != "/bucket/雪 + file" {
					t.Errorf("unexpected cleanup request %s", r.URL.String())
				}
				w.Header().Set("Content-Type", "application/xml")
				if change == "denied" {
					w.WriteHeader(403)
					fmt.Fprint(w, "<Error><Code>AccessDenied</Code><Message>denied</Message></Error>")
				} else if change == "no-such-upload" || r.Method == "GET" {
					w.WriteHeader(404)
					fmt.Fprint(w, "<Error><Code>NoSuchUpload</Code><Message>gone</Message></Error>")
				} else if r.Method == "DELETE" {
					w.WriteHeader(204)
				} else {
					t.Errorf("unexpected method %s", r.Method)
				}
			}))
			defer server.Close()
			temporary, db := openTempRegistry(t, filepath.Join(t.TempDir(), "agent.db"))
			if _, err := temporary.Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			pathStyle, v2 := true, false
			config := storageconfig.Config{Alias: "saved", Endpoint: server.URL, Region: "us-east-1", AccessKey: "private-access-value", SecretKey: "private-secret-value", PathStyleEnable: &pathStyle, UseV2SignerType: &v2}
			configs := storageconfig.NewAgent(db)
			if _, err := configs.ApplySnapshot(storageconfig.Snapshot{Revision: 1, Connections: []storageconfig.Config{config}}); err != nil {
				t.Fatal(err)
			}
			factory := &storageconfig.Factory{Source: configs, HTTPClient: server.Client(), RetryMaxAttempts: 1}
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
			record := transferstore.S3UploadRecord{Owner: registry.OwnerID(), ID: strings.Repeat("a", 32), Connection: client.(*transferstore.S3).ConnectionIdentity("saved"), Bucket: "bucket", Key: "雪 + file", UploadID: "owned-upload"}
			client.Close()
			if err := registry.RegisterUpload(context.Background(), record); err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(uploadRecords(t, db))
			if strings.Contains(string(encoded), config.SecretKey) || strings.Contains(string(encoded), config.AccessKey) {
				t.Fatal("upload journal copied credentials")
			}
			current := config
			switch change {
			case "endpoint":
				current.Endpoint = "http://127.0.0.1:1"
			case "region":
				current.Region = "different-region"
			case "signature":
				truth := true
				current.UseV2SignerType = &truth
			case "pathStyle":
				falsity := false
				current.PathStyleEnable = &falsity
			case "accessKey":
				current.AccessKey += "-new"
			case "secretKey":
				current.SecretKey += "-new"
			case "token":
				current.SessionToken = "different-session"
			case "owner":
				record.Owner = strings.Repeat("b", 64)
				if err := db.Update(func(tx *state.Tx) error { return tx.Put(s3UploadBucket, record.ID, record) }); err != nil {
					t.Fatal(err)
				}
			}
			connections := []storageconfig.Config{current}
			if change == "missing" {
				connections = nil
			}
			if _, err := configs.ApplySnapshot(storageconfig.Snapshot{Revision: 2, Connections: connections}); err != nil {
				t.Fatal(err)
			}
			restarted, err := NewS3RecoveryRegistry(db, factory, temporary)
			if err != nil {
				t.Fatal(err)
			}
			report, err := restarted.Recover(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if change == "unchanged" || change == "no-such-upload" {
				if report.Removed != 1 || len(report.Retained) != 0 || len(uploadRecords(t, db)) != 0 || requests.Load() == 0 {
					t.Fatalf("cleanup not confirmed: %+v requests=%d", report, requests.Load())
				}
			} else {
				if report.Removed != 0 || len(report.Retained) != 1 || len(uploadRecords(t, db)) != 1 {
					t.Fatalf("unsafe record cleared: %+v", report)
				}
				if change != "denied" && requests.Load() != 0 {
					t.Fatal("cleanup contacted a changed or foreign connection")
				}
			}
		})
	}
}
