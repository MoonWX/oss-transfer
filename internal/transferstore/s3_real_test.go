package transferstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// This opt-in acceptance test talks to a separately running, authenticated S3
// process. It only accepts loopback endpoints and creates uniquely named buckets.
// The launch harness supplies credentials; nothing uses an ambient cloud account.
func TestRealS3StoreAcceptance(t *testing.T) {
	endpoint := os.Getenv("OSS_TRANSFER_STORE_QA_ENDPOINT")
	if endpoint == "" {
		t.Skip("start the isolated real-S3 acceptance harness")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "::1" && parsed.Hostname() != "localhost") {
		t.Fatal("real-store acceptance requires a loopback endpoint")
	}
	config := S3Config{Endpoint: endpoint, Region: "us-east-1", AccessKey: os.Getenv("OSS_TRANSFER_STORE_QA_ACCESS_KEY"), SecretKey: os.Getenv("OSS_TRANSFER_STORE_QA_SECRET_KEY"), PathStyle: true, PartSize: minPartSize, PartConcurrency: 4, MaxBufferedBytes: 20 * 1024 * 1024}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if _, err = ListBuckets(ctx, config); err != nil {
		t.Fatalf("signed S3 readiness failed: %v", err)
	}
	invalid := config
	invalid.SecretKey = "deliberately-wrong-test-secret"
	if _, err = ListBuckets(ctx, invalid); err == nil {
		t.Fatal("real S3 service accepted an invalid signature")
	}
	var nonce [8]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	prefix := "go-store-qa-" + hex.EncodeToString(nonce[:])
	sourceConfig, targetConfig := config, config
	sourceConfig.Bucket = prefix + "-source"
	targetConfig.Bucket = prefix + "-target"
	source, err := NewS3(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	target, err := NewS3(targetConfig)
	if err != nil {
		t.Fatal(err)
	}
	for _, store := range []*S3{source, target} {
		if _, err = store.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(store.bucket)}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cleanupContext, done := context.WithTimeout(context.Background(), 30*time.Second)
			defer done()
			if err := removeQABucket(cleanupContext, store); err != nil {
				t.Errorf("isolated bucket cleanup: %v", err)
			}
		})
	}
	root := os.Getenv("OSS_TRANSFER_STORE_QA_FIXTURE")
	if root == "" {
		root = t.TempDir()
	} else {
		if err = os.MkdirAll(root, 0700); err != nil {
			t.Fatal(err)
		}
		if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
			t.Fatal("acceptance fixture directory must be empty")
		}
	}
	fixtures := map[string][]byte{"empty": {}, "unicode/中文 file + percent%.bin": []byte("Unicode path content"), "large/multipart.bin": bytes.Repeat([]byte("multipart-integrity-0123456789abcdef"), 1100000)}
	for index := 0; index < 1003; index++ {
		fixtures[fmt.Sprintf("small/%04d.bin", index)] = bytes.Repeat([]byte(fmt.Sprintf("small-%04d-", index)), 97)
	}
	for index := 0; index < 4; index++ {
		fixtures[fmt.Sprintf("medium/%02d.bin", index)] = bytes.Repeat([]byte{byte(index)}, 1024*1024)
	}
	manifest := map[string]string{}
	var total int64
	for key, data := range fixtures {
		file := filepath.Join(root, filepath.FromSlash(key))
		if err = os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(file, data, 0600); err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(data)
		manifest[key] = hex.EncodeToString(hash[:])
		total += int64(len(data))
	}
	fixtures = nil
	local, err := NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	report := map[string]any{"service": "SeaweedFS (external process)", "fixtureFiles": len(manifest), "fixtureBytes": total, "checks": []string{"invalid-signature-rejected"}}
	durations := map[string]float64{}
	report["transferSeconds"] = durations
	options := func(object Object) WriteOptions {
		return WriteOptions{Size: object.Size, Overwrite: true, ContentType: "application/octet-stream", CacheControl: "max-age=60", ContentDisposition: "attachment; filename=sample.bin", Metadata: map[string]string{"qa": "real-store", "sha256": manifest[object.Key]}}
	}
	start := time.Now()
	if err = transferRealFixtures(ctx, local, source, options, 4); err != nil {
		t.Fatal(err)
	}
	durations["local-to-s3"] = time.Since(start).Seconds()
	verifyRealBucket(t, ctx, source, manifest, true)
	rangeSeen := map[string]bool{}
	if err := source.Scan(ctx, ListOptions{Prefix: "small/", StartAfter: "small/0500.bin", Limit: 43}, func(object Object) error {
		if rangeSeen[object.Key] || object.Key <= "small/0500.bin" || !strings.HasPrefix(object.Key, "small/") {
			return fmt.Errorf("range returned a duplicate or excluded key %q", object.Key)
		}
		rangeSeen[object.Key] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for key := range manifest {
		if strings.HasPrefix(key, "small/") && key > "small/0500.bin" && !rangeSeen[key] {
			t.Fatalf("range listing omitted %q", key)
		}
	}
	destinationRoot := t.TempDir()
	destination, err := NewLocal(destinationRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	start = time.Now()
	if err = transferRealFixtures(ctx, source, destination, objectWriteOptions, 4); err != nil {
		t.Fatal(err)
	}
	durations["s3-to-local"] = time.Since(start).Seconds()
	for key, expected := range manifest {
		file, err := os.Open(filepath.Join(destinationRoot, filepath.FromSlash(key)))
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.New()
		_, err = io.Copy(hash, file)
		file.Close()
		if err != nil || hex.EncodeToString(hash.Sum(nil)) != expected {
			t.Fatalf("download integrity mismatch %q: %v", key, err)
		}
	}
	start = time.Now()
	if err = transferRealFixtures(ctx, source, target, objectWriteOptions, 4); err != nil {
		t.Fatal(err)
	}
	durations["s3-to-s3"] = time.Since(start).Seconds()
	verifyRealBucket(t, ctx, target, manifest, true)
	checkRealConflict(t, ctx, target)
	checkRealCancellation(t, ctx, target)
	v2Config := sourceConfig
	v2Config.SignatureVersion = "v2"
	v2, err := NewS3(v2Config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ListBuckets(ctx, v2Config); err != nil {
		t.Fatalf("real S3 did not accept supported V2 signing: %v", err)
	}
	v2key := "v2/中文 space+%.bin"
	v2data := bytes.Repeat([]byte("v2-payload"), 600000)
	result, err := v2.Write(ctx, v2key, bytes.NewReader(v2data), WriteOptions{Size: int64(len(v2data)), Overwrite: true, Metadata: map[string]string{"qa": "v2"}})
	if err != nil || !result.Committed {
		t.Fatalf("real V2 multipart: %+v %v", result, err)
	}
	response, err := source.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(source.bucket), Key: aws.String(v2key)})
	if err != nil {
		t.Fatal(err)
	}
	actual, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || !bytes.Equal(actual, v2data) {
		t.Fatal("V2 transfer integrity failed")
	}
	report["checks"] = []string{"invalid-signature-rejected", "local-to-s3-hash-metadata", "s3-to-local-hash", "s3-to-s3-hash-metadata", "1000-object-pagination", "skip-and-overwrite", "multipart-cancel-preserves-target", "multipart-abort-cleanup", "v2-authenticated-multipart"}
	if path := os.Getenv("OSS_TRANSFER_STORE_QA_REPORT"); path != "" {
		data, _ := json.MarshalIndent(report, "", "  ")
		if err = os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("real authenticated S3 passed: files=%d bytes=%d transfer_seconds=%v", len(manifest), total, durations)
}

func objectWriteOptions(object Object) WriteOptions {
	return WriteOptions{Size: object.Size, Overwrite: true, ContentType: object.ContentType, ContentEncoding: object.ContentEncoding, ContentDisposition: object.ContentDisposition, CacheControl: object.CacheControl, Metadata: copyMetadata(object.Metadata)}
}

func transferRealFixtures(ctx context.Context, source, destination Store, options func(Object) WriteOptions, workers int) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	queue := make(chan Object, workers)
	var group sync.WaitGroup
	var once sync.Once
	var first error
	fail := func(err error) {
		if err != nil {
			once.Do(func() { first = err; cancel() })
		}
	}
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			for object := range queue {
				reader, opened, err := source.Open(ctx, object.Key, OpenOptions{Expected: &object})
				if err != nil {
					fail(err)
					continue
				}
				_, err = destination.Write(ctx, object.Key, reader, options(opened))
				reader.Close()
				fail(err)
			}
		}()
	}
	err := source.Scan(ctx, ListOptions{Limit: 37}, func(object Object) error {
		select {
		case queue <- object:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	fail(err)
	close(queue)
	group.Wait()
	return first
}

func verifyRealBucket(t *testing.T, ctx context.Context, store *S3, manifest map[string]string, metadata bool) {
	t.Helper()
	keys := []string{}
	input := &s3.ListObjectsV2Input{Bucket: aws.String(store.bucket), MaxKeys: aws.Int32(43)}
	for {
		page, err := store.client.ListObjectsV2(ctx, input)
		if err != nil {
			t.Fatal(err)
		}
		for _, object := range page.Contents {
			keys = append(keys, aws.ToString(object.Key))
		}
		if !aws.ToBool(page.IsTruncated) {
			break
		}
		input.ContinuationToken = page.NextContinuationToken
	}
	if len(keys) != len(manifest) {
		t.Fatalf("real object set size %d != %d", len(keys), len(manifest))
	}
	sort.Strings(keys)
	for _, key := range keys {
		expected, exists := manifest[key]
		if !exists {
			t.Fatalf("unexpected real object %q", key)
		}
		response, err := store.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(store.bucket), Key: aws.String(key)})
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.New()
		_, err = io.Copy(hash, response.Body)
		response.Body.Close()
		if err != nil || hex.EncodeToString(hash.Sum(nil)) != expected {
			t.Fatalf("real object hash mismatch %q: %v", key, err)
		}
		if metadata && (response.Metadata["sha256"] != expected || response.Metadata["qa"] != "real-store" || aws.ToString(response.CacheControl) != "max-age=60" || aws.ToString(response.ContentType) != "application/octet-stream" || aws.ToString(response.ContentDisposition) != "attachment; filename=sample.bin") {
			t.Fatalf("real object metadata changed %q", key)
		}
	}
}

func checkRealConflict(t *testing.T, ctx context.Context, store *S3) {
	t.Helper()
	key := "conflict/check"
	original, replacement := []byte("original"), []byte("replacement")
	if _, err := store.Write(ctx, key, bytes.NewReader(original), WriteOptions{Size: int64(len(original)), Overwrite: true}); err != nil {
		t.Fatal(err)
	}
	skipped, err := store.Write(ctx, key, panicReader{}, WriteOptions{Size: int64(len(replacement))})
	if err != nil || !skipped.Skipped {
		t.Fatalf("real SKIP %+v %v", skipped, err)
	}
	object, err := store.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(store.bucket), Key: aws.String(key)})
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(object.Body)
	object.Body.Close()
	if err != nil || !bytes.Equal(data, original) {
		t.Fatal("SKIP replaced real target")
	}
	if result, err := store.Write(ctx, key, bytes.NewReader(replacement), WriteOptions{Size: int64(len(replacement)), Overwrite: true}); err != nil || !result.Committed {
		t.Fatalf("real OVERWRITE %+v %v", result, err)
	}
	object, err = store.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(store.bucket), Key: aws.String(key)})
	if err != nil {
		t.Fatal(err)
	}
	data, err = io.ReadAll(object.Body)
	object.Body.Close()
	if err != nil || !bytes.Equal(data, replacement) {
		t.Fatal("OVERWRITE did not replace real target")
	}
}

type slowRealInput struct {
	ctx       context.Context
	remaining int64
}

func (r *slowRealInput) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	select {
	case <-time.After(2 * time.Millisecond):
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	}
	n := int(min(int64(len(p)), min(r.remaining, 64*1024)))
	for i := range p[:n] {
		p[i] = 'x'
	}
	r.remaining -= int64(n)
	return n, nil
}
func checkRealCancellation(t *testing.T, ctx context.Context, store *S3) {
	t.Helper()
	key := "cancel/keep-existing"
	original := []byte("previous complete object")
	if _, err := store.Write(ctx, key, bytes.NewReader(original), WriteOptions{Size: int64(len(original)), Overwrite: true}); err != nil {
		t.Fatal(err)
	}
	uploadContext, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := store.Write(uploadContext, key, &slowRealInput{ctx: uploadContext, remaining: 64 * 1024 * 1024}, WriteOptions{Size: 64 * 1024 * 1024, Overwrite: true})
		done <- err
	}()
	observed := false
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		uploads, err := store.client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(store.bucket), Prefix: aws.String(key)})
		if err != nil {
			t.Fatal(err)
		}
		for _, upload := range uploads.Uploads {
			parts, err := store.client.ListParts(ctx, &s3.ListPartsInput{Bucket: aws.String(store.bucket), Key: aws.String(key), UploadId: upload.UploadId})
			if err == nil && len(parts.Parts) > 0 {
				observed = true
				break
			}
		}
		if observed {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("real cancel result %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("real cancel did not stop")
	}
	if !observed {
		t.Fatal("did not observe an uploaded real multipart part before canceling")
	}
	uploads, err := store.client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(store.bucket), Prefix: aws.String(key)})
	if err != nil || len(uploads.Uploads) != 0 {
		t.Fatalf("real incomplete multipart upload leaked: %v", err)
	}
	object, err := store.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(store.bucket), Key: aws.String(key)})
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(object.Body)
	object.Body.Close()
	if err != nil || !bytes.Equal(data, original) {
		t.Fatal("cancellation exposed a partial target")
	}
}

func removeQABucket(ctx context.Context, store *S3) error {
	if !strings.HasPrefix(store.bucket, "go-store-qa-") {
		return errors.New("refuse to clean a non-QA bucket")
	}
	for {
		page, err := store.client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(store.bucket), MaxUploads: aws.Int32(1000)})
		if err != nil {
			return err
		}
		if len(page.Uploads) == 0 {
			break
		}
		for _, upload := range page.Uploads {
			if _, err := store.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String(store.bucket), Key: upload.Key, UploadId: upload.UploadId}); err != nil {
				return err
			}
		}
	}
	for {
		page, err := store.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(store.bucket), MaxKeys: aws.Int32(1000)})
		if err != nil {
			return err
		}
		if len(page.Contents) == 0 {
			break
		}
		objects := make([]types.ObjectIdentifier, 0, len(page.Contents))
		for _, object := range page.Contents {
			objects = append(objects, types.ObjectIdentifier{Key: object.Key})
		}
		out, err := store.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String(store.bucket), Delete: &types.Delete{Objects: objects, Quiet: aws.Bool(true)}})
		if err != nil {
			return err
		}
		if len(out.Errors) > 0 {
			return errors.New("some QA objects could not be removed")
		}
	}
	_, err := store.client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(store.bucket)})
	return err
}
