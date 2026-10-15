package agentservice

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

const recoveryPartSize = 9 * 1024 * 1024
const recoveryObjectKey = "crash/雪 + partial.bin"

type mpuCrashReader struct{ written int }

func (r *mpuCrashReader) Read(buffer []byte) (int, error) {
	if r.written >= recoveryPartSize+32*1024 {
		fmt.Println("READY")
		for {
			time.Sleep(time.Hour)
		}
	}
	n := min(len(buffer), recoveryPartSize+32*1024-r.written)
	for i := range buffer[:n] {
		buffer[i] = byte((r.written + i) % 251)
	}
	r.written += n
	return n, nil
}

func TestMPUCrashHelper(t *testing.T) {
	if os.Getenv("OSS_TRANSFER_MPU_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	db, err := state.Open(os.Getenv("OSS_TRANSFER_MPU_DB"))
	if err != nil {
		t.Fatal(err)
	}
	temporary, err := NewLocalTempRegistry(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := temporary.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	factory := &storageconfig.Factory{Source: storageconfig.NewAgent(db), PartSize: recoveryPartSize, PartConcurrency: 4, SpoolDirectory: os.Getenv("OSS_TRANSFER_MPU_SPOOL")}
	registry, err := NewS3RecoveryRegistry(db, factory, temporary)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	job := execution.Job{ParentID: 1, Source: execution.Endpoint{Kind: "S3", Ref: "crash-test", Bucket: os.Getenv("OSS_TRANSFER_MPU_BUCKET")}, Destination: execution.Endpoint{Kind: "S3", Ref: "crash-test", Bucket: os.Getenv("OSS_TRANSFER_MPU_BUCKET")}}
	source, target, err := registry.Factory(factory).Open(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	defer target.Close()
	_, err = target.Write(context.Background(), recoveryObjectKey, &mpuCrashReader{}, transferstore.WriteOptions{Size: 3 * recoveryPartSize, Overwrite: true})
	t.Fatalf("crash helper unexpectedly returned: %v", err)
}

// This test talks to a real separately running SeaweedFS service. The subprocess
// uses the same durable Factory/registries as Agent, then dies without defers.
// It is not JDCloud compatibility evidence or a multipart-resume implementation.
func TestRealS3CrashRecovery(t *testing.T) {
	endpoint := os.Getenv("OSS_TRANSFER_STORE_QA_ENDPOINT")
	if endpoint == "" {
		t.Skip("start the isolated real-S3 acceptance harness")
	}
	if !recoveryAcceptanceEndpointAllowed(endpoint, os.Getenv("OSS_TRANSFER_STORE_QA_HOST_FIXTURE") == "1") {
		t.Fatal("recovery acceptance requires a loopback endpoint or the explicitly enabled VM host fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pathStyle, v2 := true, false
	config := storageconfig.Config{Alias: "crash-test", Endpoint: endpoint, Region: "us-east-1", AccessKey: os.Getenv("OSS_TRANSFER_STORE_QA_ACCESS_KEY"), SecretKey: os.Getenv("OSS_TRANSFER_STORE_QA_SECRET_KEY"), PathStyleEnable: &pathStyle, UseV2SignerType: &v2}
	client := s3.NewFromConfig(aws.Config{Region: config.Region, Credentials: credentials.NewStaticCredentialsProvider(config.AccessKey, config.SecretKey, "")}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	bucket := "go-mpu-crash-" + hex.EncodeToString(nonce[:])
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		uploads, err := client.ListMultipartUploads(cleanup, &s3.ListMultipartUploadsInput{Bucket: aws.String(bucket)})
		if err == nil {
			for _, upload := range uploads.Uploads {
				client.AbortMultipartUpload(cleanup, &s3.AbortMultipartUploadInput{Bucket: aws.String(bucket), Key: upload.Key, UploadId: upload.UploadId})
			}
		}
		client.DeleteObject(cleanup, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(recoveryObjectKey)})
		if _, err := client.DeleteBucket(cleanup, &s3.DeleteBucketInput{Bucket: aws.String(bucket)}); err != nil {
			t.Errorf("test bucket cleanup failed: %v", err)
		}
	}()
	old := []byte("previous complete object")
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(recoveryObjectKey), Body: bytes.NewReader(old)}); err != nil {
		t.Fatal(err)
	}
	foreign, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String("unregistered-other-client")})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	dbPath, spool := filepath.Join(root, "agent.db"), filepath.Join(root, "spool")
	db, err := state.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storageconfig.NewAgent(db).ApplySnapshot(storageconfig.Snapshot{Revision: 1, Connections: []storageconfig.Config{config}}); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	command := exec.Command(os.Args[0], "-test.run=^TestMPUCrashHelper$", "-test.v")
	command.Env = append(os.Environ(), "OSS_TRANSFER_MPU_HELPER=1", "OSS_TRANSFER_MPU_DB="+dbPath, "OSS_TRANSFER_MPU_SPOOL="+spool, "OSS_TRANSFER_MPU_BUCKET="+bucket)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	helper := &tempHelper{command: command}
	defer helper.kill()
	ready := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if scanner.Text() == "READY" {
				ready <- true
				return
			}
		}
		ready <- false
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("helper exited before writing incomplete spool")
		}
	case <-ctx.Done():
		t.Fatal("helper did not reach crash point")
	}
	var uploadID string
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		list, err := client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(bucket)})
		if err != nil {
			t.Fatal(err)
		}
		for _, upload := range list.Uploads {
			if aws.ToString(upload.Key) != recoveryObjectKey {
				continue
			}
			parts, err := client.ListParts(ctx, &s3.ListPartsInput{Bucket: aws.String(bucket), Key: upload.Key, UploadId: upload.UploadId})
			if err == nil && len(parts.Parts) == 1 && aws.ToInt64(parts.Parts[0].Size) == recoveryPartSize {
				uploadID = aws.ToString(upload.UploadId)
			}
		}
		if uploadID != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if uploadID == "" {
		t.Fatal("no real remote multipart part committed before crash")
	}
	helper.kill()
	temporary, db := openTempRegistry(t, dbPath)
	if records := uploadRecords(t, db); len(records) != 1 || records[0].UploadID != uploadID {
		t.Fatal("uploaded part has no durable upload record")
	}
	if records := tempRecords(t, db); len(records) == 0 {
		t.Fatal("incomplete spool has no durable ownership record")
	}
	local, err := temporary.Recover(ctx)
	if err != nil || local.Removed < 1 || len(local.Retained) != 0 {
		t.Fatalf("spool recovery failed: %+v %v", local, err)
	}
	factory := &storageconfig.Factory{Source: storageconfig.NewAgent(db)}
	registry, err := NewS3RecoveryRegistry(db, factory, temporary)
	if err != nil {
		t.Fatal(err)
	}
	report, err := registry.Recover(ctx)
	if err != nil || report.Removed != 1 || len(report.Retained) != 0 || len(uploadRecords(t, db)) != 0 || len(tempRecords(t, db)) != 0 {
		t.Fatalf("remote recovery failed: %+v %v", report, err)
	}
	if files, err := os.ReadDir(spool); err != nil || len(files) != 0 {
		t.Fatalf("orphan spool remains: %v %v", files, err)
	}
	list, err := client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(bucket)})
	if err != nil || len(list.Uploads) != 1 || aws.ToString(list.Uploads[0].UploadId) != aws.ToString(foreign.UploadId) {
		t.Fatalf("cleanup removed foreign upload or retained owned upload: %v %v", list, err)
	}
	result, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(recoveryObjectKey)})
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(result.Body)
	result.Body.Close()
	if err != nil || !bytes.Equal(content, old) {
		t.Fatal("recovery damaged previous completed destination")
	}
	t.Logf("real SIGKILL: %d MiB part committed, %d owned spool files removed, owned MPU removed, foreign MPU and old object preserved", recoveryPartSize/(1024*1024), local.Removed)
}

func recoveryAcceptanceEndpointAllowed(endpoint string, hostFixture bool) bool {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.Opaque != "" ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawPath != "" ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return false
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 {
		return false
	}
	// The pinned FreeBSD VM's user-mode network maps this one address to the
	// runner's loopback interface. Never turn this opt-in into arbitrary S3 access.
	return parsed.Hostname() == "127.0.0.1" || (hostFixture && parsed.Hostname() == "192.168.122.2")
}

func TestRecoveryAcceptanceEndpointGuard(t *testing.T) {
	for _, test := range []struct {
		name, endpoint string
		hostFixture    bool
		allowed        bool
	}{
		{"loopback", "http://127.0.0.1:19444", false, true},
		{"loopback with opt-in", "http://127.0.0.1:19444/", true, true},
		{"VM host explicitly enabled", "http://192.168.122.2:19444", true, true},
		{"VM host without opt-in", "http://192.168.122.2:19444", false, false},
		{"public IP with opt-in", "http://1.2.3.4:19444", true, false},
		{"other private IP with opt-in", "http://192.168.122.3:19444", true, false},
		{"DNS alias", "http://localhost:19444", true, false},
		{"credentials on loopback", "http://key:secret@127.0.0.1:19444", false, false},
		{"credentials on VM host", "http://key:secret@192.168.122.2:19444", true, false},
		{"userinfo host trick", "http://192.168.122.2@1.2.3.4:19444", true, false},
		{"wrong scheme", "https://192.168.122.2:19444", true, false},
		{"missing port", "http://192.168.122.2", true, false},
		{"zero port", "http://192.168.122.2:0", true, false},
		{"out of range port", "http://192.168.122.2:65536", true, false},
		{"invalid port", "http://192.168.122.2:bad", true, false},
		{"bucket path", "http://192.168.122.2:19444/bucket", true, false},
		{"query", "http://192.168.122.2:19444?key=secret", true, false},
		{"empty query", "http://192.168.122.2:19444?", true, false},
		{"fragment", "http://192.168.122.2:19444#secret", true, false},
		{"IPv6 address", "http://[::1]:19444", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if actual := recoveryAcceptanceEndpointAllowed(test.endpoint, test.hostFixture); actual != test.allowed {
				t.Fatalf("endpoint acceptance = %v, want %v", actual, test.allowed)
			}
		})
	}
}
