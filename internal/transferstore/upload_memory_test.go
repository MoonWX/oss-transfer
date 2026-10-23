package transferstore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

func memoryState(b *UploadMemoryBudget) (int64, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used, b.waiting.Len()
}

func budgetStore(t *testing.T, server *protocolS3, budget *UploadMemoryBudget, spool string) *S3 {
	t.Helper()
	s, err := NewS3(S3Config{Endpoint: server.server.URL, Bucket: "test-bucket", AccessKey: "test-access", SecretKey: "fake-test-secret", PathStyle: true, PartSize: minPartSize, PartConcurrency: 3, MaxBufferedBytes: 15 << 20, UploadMemory: budget, SpoolDirectory: spool, HTTPClient: server.server.Client(), RetryMaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Two independent S3 clients upload real payloads over HTTP, including a retry.
// Neither can read/allocate a new part while the shared budget is occupied.
func TestUploadMemorySharedAcrossConcurrentClientsAndRetries(t *testing.T) {
	budget, err := NewUploadMemoryBudget(2 * minPartSize)
	if err != nil {
		t.Fatal(err)
	}
	servers := []*protocolS3{newProtocolS3(t), newProtocolS3(t)}
	servers[0].failFirstPart = true
	arrivals := make(chan struct{}, 20)
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	spool := t.TempDir()
	payload := bytes.Repeat([]byte("p"), int(2*minPartSize+7))
	inputs := []*countedReader{{reader: bytes.NewReader(payload)}, {reader: bytes.NewReader(payload)}}
	done := make(chan error, 2)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for i, server := range servers {
		server.onPart = func(int) { arrivals <- struct{}{}; <-release }
		store := budgetStore(t, server, budget, spool)
		go func(i int) {
			result, err := store.Write(ctx, "shared", inputs[i], WriteOptions{Size: int64(len(payload)), Overwrite: true})
			if err == nil && !result.Committed {
				err = errors.New("upload did not commit")
			}
			done <- err
		}(i)
	}
	for range 2 {
		select {
		case <-arrivals:
		case <-ctx.Done():
			t.Fatal("shared budget never admitted two parts")
		}
	}
	if used, _ := memoryState(budget); used != 2*minPartSize {
		t.Fatal("unexpected payload reservation", used)
	}
	select {
	case <-arrivals:
		t.Fatal("third part exceeded the shared memory budget")
	case <-time.After(100 * time.Millisecond):
	}
	if read := inputs[0].count.Load() + inputs[1].count.Load(); read != 2*minPartSize {
		t.Fatal("source read ahead of the memory budget", read)
	}
	if files, err := os.ReadDir(spool); err != nil || len(files) != 0 {
		t.Fatal("a full memory budget spilled to disk", files, err)
	}
	once.Do(func() { close(release) })
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if used, waiting := memoryState(budget); used != 0 || waiting != 0 {
		t.Fatal("completed uploads leaked memory reservations", used, waiting)
	}
	for i, server := range servers {
		server.mu.Lock()
		equal, attempts := bytes.Equal(server.objects["shared"].data, payload), server.partAttempts[1]
		server.mu.Unlock()
		if !equal || i == 0 && attempts != 2 {
			t.Fatal("shared budget broke integrity or retry", i, attempts)
		}
	}
}

func TestUploadMemoryCancellationReleasesActiveAndWaitingParts(t *testing.T) {
	budget, err := NewUploadMemoryBudget(minPartSize)
	if err != nil {
		t.Fatal(err)
	}
	blocked := newProtocolS3(t)
	blocked.blockPart = true
	spool := t.TempDir()
	store := budgetStore(t, blocked, budget, spool)
	payload := bytes.Repeat([]byte("x"), int(minPartSize+7))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	active := make(chan error, 1)
	go func() {
		_, err := store.Write(ctx, "active", bytes.NewReader(payload), WriteOptions{Size: int64(len(payload)), Overwrite: true})
		active <- err
	}()
	select {
	case <-blocked.partStarted:
	case <-ctx.Done():
		t.Fatal("active upload did not enter")
	}
	// Both single PUT and multipart wait before consuming source content.
	for _, size := range []int64{minPartSize, minPartSize + 7} {
		waitingServer := newProtocolS3(t)
		waitingStore := budgetStore(t, waitingServer, budget, spool)
		input := &countedReader{reader: bytes.NewReader(payload[:size])}
		waitCtx, cancelWait := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() {
			_, err := waitingStore.Write(waitCtx, "waiting", input, WriteOptions{Size: size, Overwrite: true})
			done <- err
		}()
		deadline := time.Now().Add(5 * time.Second)
		for {
			_, waiting := memoryState(budget)
			if waiting >= 2 {
				break
			} // The active upload also awaits its last part.
			if time.Now().After(deadline) {
				cancelWait()
				t.Fatal("second upload never reached memory backpressure")
			}
			time.Sleep(time.Millisecond)
		}
		cancelWait()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal("memory wait ignored cancellation", err)
		}
		if input.count.Load() != 0 {
			t.Fatal("waiting upload consumed payload")
		}
		if used, _ := memoryState(budget); used != minPartSize {
			t.Fatal("cancellation changed another upload's reservation", used)
		}
	}
	cancel()
	if err := <-active; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if used, waiting := memoryState(budget); used != 0 || waiting != 0 {
		t.Fatal("cancelled upload leaked reservations", used, waiting)
	}
	if files, err := os.ReadDir(spool); err != nil || len(files) != 0 {
		t.Fatal("cancellation left spool files", files, err)
	}
}
