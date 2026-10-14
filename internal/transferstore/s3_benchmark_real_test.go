package transferstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type prefixStore struct {
	Store
	prefix string
}

func (s prefixStore) Write(ctx context.Context, key string, input io.Reader, options WriteOptions) (WriteResult, error) {
	return s.Store.Write(ctx, s.prefix+key, input, options)
}

// This records the real release JAR and Go I/O layer on the same host, S3 process,
// fixtures, and 4-worker/4-part configuration. The JAR includes orchestration and
// durable task bookkeeping; the Store does not. Results do not prove end-to-end
// Go Master/Agent performance equivalence and must not be presented as such.
func TestRealS3JavaBaseline(t *testing.T) {
	origin := os.Getenv("OSS_TRANSFER_STORE_QA_JAVA_URL")
	if origin == "" {
		t.Skip("run the real-S3 harness with --java and --java-jar")
	}
	endpoint := os.Getenv("OSS_TRANSFER_STORE_QA_ENDPOINT")
	for _, value := range []string{origin, endpoint} {
		parsed, err := url.Parse(value)
		if err != nil || (parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "::1" && parsed.Hostname() != "localhost") {
			t.Fatal("benchmark requires loopback services")
		}
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	config := S3Config{Endpoint: endpoint, Region: "us-east-1", Bucket: "go-store-qa-bench-" + hex.EncodeToString(nonce[:]), AccessKey: os.Getenv("OSS_TRANSFER_STORE_QA_ACCESS_KEY"), SecretKey: os.Getenv("OSS_TRANSFER_STORE_QA_SECRET_KEY"), PathStyle: true, PartSize: minPartSize, PartConcurrency: 4, MaxBufferedBytes: 20 * 1024 * 1024}
	ctx, cancel := context.WithTimeout(context.Background(), 14*time.Minute)
	defer cancel()
	store, err := NewS3(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(store.bucket)}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		if err := removeQABucket(cleanup, store); err != nil {
			t.Errorf("benchmark bucket cleanup: %v", err)
		}
	})
	results := []map[string]any{}
	for _, fixture := range []struct {
		name        string
		files, size int
	}{{"single-small", 1, 64 * 1024}, {"many-small", 256, 32 * 1024}, {"single-large", 1, 64 * 1024 * 1024}, {"many-large", 4, 16 * 1024 * 1024}} {
		directory := t.TempDir()
		manifest := map[string]string{}
		for index := 0; index < fixture.files; index++ {
			key := fmt.Sprintf("%04d.bin", index)
			file, err := os.Create(filepath.Join(directory, key))
			if err != nil {
				t.Fatal(err)
			}
			block := make([]byte, 64*1024)
			for offset := range block {
				block[offset] = byte((offset*29 + index*17) % 251)
			}
			hash := sha256.New()
			writer := io.MultiWriter(file, hash)
			for remaining := fixture.size; remaining > 0; {
				size := min(remaining, len(block))
				if _, err = writer.Write(block[:size]); err != nil {
					t.Fatal(err)
				}
				remaining -= size
			}
			if err = file.Close(); err != nil {
				t.Fatal(err)
			}
			manifest[key] = hex.EncodeToString(hash.Sum(nil))
		}
		source, err := NewLocal(directory)
		if err != nil {
			t.Fatal(err)
		}
		for repetition := 0; repetition < 3; repetition++ {
			engines := []string{"go-store", "java-release"}
			if repetition%2 == 1 {
				engines[0], engines[1] = engines[1], engines[0]
			}
			for _, engine := range engines {
				prefix := fmt.Sprintf("%s/%s/%d/", fixture.name, engine, repetition)
				start := time.Now()
				var task map[string]any
				if engine == "go-store" {
					err = transferRealFixtures(ctx, source, prefixStore{Store: store, prefix: prefix}, func(object Object) WriteOptions { return WriteOptions{Size: object.Size, Overwrite: true} }, 4)
				} else {
					task, err = runJavaTransfer(ctx, origin, os.Getenv("OSS_TRANSFER_STORE_QA_JAVA_TOKEN"), directory, store.bucket, prefix)
				}
				duration := time.Since(start).Seconds()
				if err != nil {
					t.Fatalf("%s %s: %v", fixture.name, engine, err)
				}
				for key, expected := range manifest {
					response, err := store.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(store.bucket), Key: aws.String(prefix + key)})
					if err != nil {
						t.Fatalf("verify %s %s: %v", engine, key, err)
					}
					hash := sha256.New()
					_, err = io.Copy(hash, response.Body)
					response.Body.Close()
					if err != nil || hex.EncodeToString(hash.Sum(nil)) != expected {
						t.Fatalf("benchmark integrity failed for %s %s", engine, key)
					}
				}
				item := map[string]any{"fixture": fixture.name, "engine": engine, "repetition": repetition + 1, "files": fixture.files, "bytes": fixture.files * fixture.size, "seconds": duration, "MiBPerSecond": float64(fixture.files*fixture.size) / (1024 * 1024) / duration}
				if task != nil {
					item["taskId"] = task["taskIndex"]
					item["taskStart"] = task["startTime"]
					item["taskEnd"] = task["endTime"]
				}
				results = append(results, item)
				t.Logf("%s %s iteration=%d elapsed=%.3fs throughput=%.2f MiB/s", fixture.name, engine, repetition+1, duration, item["MiBPerSecond"])
			}
		}
		source.Close()
	}
	report := map[string]any{"measurement": "Go Store I/O versus Java release full task, same host/S3/fixtures; Java includes task orchestration. This is not end-to-end Go performance equivalence.", "workers": 4, "partConcurrency": 4, "partBytes": minPartSize, "runs": results}
	if path := os.Getenv("OSS_TRANSFER_STORE_QA_BENCH_REPORT"); path != "" {
		data, _ := json.MarshalIndent(report, "", "  ")
		if err = os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func runJavaTransfer(ctx context.Context, origin, token, directory, bucket, prefix string) (map[string]any, error) {
	request := []map[string]any{{"direction": "LOCAL_TO_OSS", "sourceEndpoint": map[string]any{"type": "LOCAL", "localPath": directory, "executionNodeIp": "127.0.0.1", "nodeBindings": []map[string]string{{"executionNodeIp": "127.0.0.1", "localPath": directory}}}, "destinationEndpoint": map[string]any{"type": "OSS", "s3Alias": "go-store-qa", "bucket": bucket, "prefix": prefix}, "conflictPolicy": "OVERWRITE", "distributionMode": "BALANCED"}}
	response, err := javaAPI(ctx, origin, token, "/api/master/local-transfers", request)
	if err != nil {
		return nil, err
	}
	data, ok := response["data"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("Java creation response has no data")
	}
	ids, ok := data["parentTaskIds"].([]any)
	if !ok || len(ids) != 1 {
		return nil, fmt.Errorf("Java did not create exactly one task")
	}
	id, ok := ids[0].(float64)
	if !ok {
		return nil, fmt.Errorf("Java task ID is not numeric")
	}
	deadline := time.NewTimer(90 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("Java task %.0f did not finish", id)
		case <-ticker.C:
		}
		response, err = javaAPI(ctx, origin, token, "/api/master/status?includeSubtasks=false", nil)
		if err != nil {
			return nil, err
		}
		if partial, _ := response["partial"].(bool); partial {
			return nil, fmt.Errorf("Java status is partial")
		}
		tasks, _ := response["tasks"].([]any)
		for _, entry := range tasks {
			task, ok := entry.(map[string]any)
			if !ok || task["taskIndex"] != id {
				continue
			}
			status, _ := task["status"].(string)
			switch status {
			case "COMPLETED":
				if failed, _ := task["failedObjectCount"].(float64); failed != 0 {
					return nil, fmt.Errorf("Java completed with %.0f failures", failed)
				}
				return task, nil
			case "FAILED", "PARTIAL_FAILED", "STOPPED":
				return nil, fmt.Errorf("Java task %.0f ended as %s", id, status)
			}
		}
	}
}

func javaAPI(ctx context.Context, origin, token, path string, body any) (map[string]any, error) {
	method := "GET"
	var data io.Reader
	if body != nil {
		method = "POST"
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		data = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, origin+path, data)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	var result map[string]any
	if err = json.NewDecoder(response.Body).Decode(&result); err != nil {
		return nil, err
	}
	if response.StatusCode >= 400 {
		return nil, fmt.Errorf("Java %s %s status %d: %v", method, path, response.StatusCode, result["message"])
	}
	if success, exists := result["success"]; exists && success == false {
		return nil, fmt.Errorf("Java API failed: %v", result["message"])
	}
	return result, nil
}
