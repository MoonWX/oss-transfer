package transferstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// TestRealS3RangedLocalDownload downloads one large object from the isolated
// real-S3 harness (see TestRealS3StoreAcceptance) into a Local target, first
// as one stream and then in ranges. It checks every downloaded file's SHA-256
// and logs the time of each mode; it asserts integrity, not speed.
//
//	OSS_TRANSFER_STORE_QA_RANGED_MIB=1024 go test ./internal/transferstore -run TestRealS3RangedLocalDownload -v
func TestRealS3RangedLocalDownload(t *testing.T) {
	endpoint := os.Getenv("OSS_TRANSFER_STORE_QA_ENDPOINT")
	if endpoint == "" {
		t.Skip("start the isolated real-S3 acceptance harness")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "::1" && parsed.Hostname() != "localhost") {
		t.Fatal("real-store acceptance requires a loopback endpoint")
	}
	mebibytes := 512
	if value, err := strconv.Atoi(os.Getenv("OSS_TRANSFER_STORE_QA_RANGED_MIB")); err == nil && value > 128 {
		mebibytes = value
	}
	size := int64(mebibytes) << 20
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	budget, err := NewUploadMemoryBudget(DefaultUploadMemoryBytes)
	if err != nil {
		t.Fatal(err)
	}
	source, err := NewS3(S3Config{Endpoint: endpoint, Region: "us-east-1", Bucket: "go-store-qa-" + hex.EncodeToString(nonce[:]) + "-ranged", AccessKey: os.Getenv("OSS_TRANSFER_STORE_QA_ACCESS_KEY"), SecretKey: os.Getenv("OSS_TRANSFER_STORE_QA_SECRET_KEY"), PathStyle: true, PartSize: 16 << 20, UploadMemory: budget})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	if _, err := source.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(source.bucket)}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		if err := removeQABucket(cleanup, source); err != nil {
			t.Errorf("isolated bucket cleanup: %v", err)
		}
	})
	payload := make([]byte, size)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(payload)
	if _, err := source.Write(ctx, "large.bin", bytes.NewReader(payload), WriteOptions{Size: size, Overwrite: true}); err != nil {
		t.Fatal(err)
	}
	payload = nil
	object, err := source.Stat(ctx, "large.bin")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []struct {
		name string
		min  int64
		idle int
	}{{"stream", -1, 0}, {"ranged-base4", 0, 0}, {"ranged-idle16", 0, 100}} {
		root := t.TempDir()
		target, err := NewLocal(root)
		if err != nil {
			t.Fatal(err)
		}
		lender := NewPartLender()
		lender.SetIdle(func() int { return mode.idle })
		if err := target.SetRangedWrites(LocalRangedOptions{MinSize: mode.min, PartLender: lender}); err != nil {
			t.Fatal(err)
		}
		var times []time.Duration
		for range 3 {
			began := time.Now()
			options := WriteOptions{Size: size, Overwrite: true}
			var input io.ReadCloser
			if target.RangedWrite(size) {
				ranges, _, err := source.OpenRanges(ctx, "large.bin", OpenOptions{Expected: &object})
				if err != nil {
					t.Fatal(err)
				}
				options.Ranges, input = ranges.OpenRange, io.NopCloser(strings.NewReader(""))
				defer ranges.Close()
			} else if input, _, err = source.Open(ctx, "large.bin", OpenOptions{Expected: &object}); err != nil {
				t.Fatal(err)
			}
			result, err := target.Write(ctx, "large.bin", input, options)
			_ = input.Close()
			if err != nil || !result.Committed {
				t.Fatal(mode.name, result, err)
			}
			times = append(times, time.Since(began))
			file, err := os.Open(filepath.Join(root, "large.bin"))
			if err != nil {
				t.Fatal(err)
			}
			hash := sha256.New()
			_, err = io.Copy(hash, file)
			file.Close()
			if err != nil || !bytes.Equal(hash.Sum(nil), want[:]) {
				t.Fatalf("%s: downloaded content differs (%v)", mode.name, err)
			}
		}
		best := times[0]
		for _, elapsed := range times {
			best = min(best, elapsed)
		}
		t.Logf("%-14s %d MiB: runs %v, best %.0f MiB/s", mode.name, mebibytes, times, float64(mebibytes)/best.Seconds())
		_ = target.Close()
	}
}
