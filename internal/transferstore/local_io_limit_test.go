package transferstore

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
)

type countingLocalIO struct {
	mu         sync.Mutex
	operations int
	bytes      int
}

func (c *countingLocalIO) WaitLocalOperation(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.operations++
	return ctx.Err()
}

func (c *countingLocalIO) WaitLocalBytes(ctx context.Context, n int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bytes += n
	return ctx.Err()
}

func (c *countingLocalIO) take() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	operations, n := c.operations, c.bytes
	c.operations, c.bytes = 0, 0
	return operations, n
}

// Every local access is charged: a write, a read and a range are counted in
// bytes, and the opens, lookups and directory reads behind them in operations.
func TestLocalAccessIsChargedToTheLocalLimits(t *testing.T) {
	local, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	limiter := &countingLocalIO{}
	local.SetLocalIOLimiter(limiter)
	ctx := context.Background()
	body := bytes.Repeat([]byte("local"), 200_000) // 1,000,000 bytes, several chunks
	if _, err := local.Write(ctx, "dir/file.bin", bytes.NewReader(body), WriteOptions{Size: int64(len(body)), Overwrite: true}); err != nil {
		t.Fatal(err)
	}
	if operations, n := limiter.take(); operations < 1 || n != len(body) {
		t.Fatalf("write charged %d operations and %d bytes", operations, n)
	}
	reader, _, err := local.Open(ctx, "dir/file.bin", OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	read, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || !bytes.Equal(read, body) {
		t.Fatalf("read %d bytes: %v", len(read), err)
	}
	if operations, n := limiter.take(); operations != 1 || n != len(body) {
		t.Fatalf("read charged %d operations and %d bytes", operations, n)
	}
	ranges, _, err := local.OpenRanges(ctx, "dir/file.bin", OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	part, err := ranges.OpenRange(ctx, 10, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := io.Copy(io.Discard, part); err != nil || n != 1000 {
		t.Fatalf("range read %d: %v", n, err)
	}
	part.Close()
	ranges.Close()
	if operations, n := limiter.take(); operations != 1 || n != 1000 {
		t.Fatalf("range charged %d operations and %d bytes", operations, n)
	}
	if _, err := local.Stat(ctx, "dir/file.bin"); err != nil {
		t.Fatal(err)
	}
	if operations, _ := limiter.take(); operations != 1 {
		t.Fatalf("lookup charged %d operations", operations)
	}
	// The root: its open and two directory reads, the second one at the end of
	// the names. dir: its lookup, its own root, its open and two reads.
	// file.bin: its lookup. Then dir's link is checked as it is left, since
	// the page holds a file found below it: 3 + 5 + 1 + 1.
	if err := local.ScanPages(ctx, ListOptions{Limit: 10}, func(Page) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if operations, _ := limiter.take(); operations != 10 {
		t.Fatalf("scan charged %d operations", operations)
	}
}

// A refused budget (a cancelled task) stops the local access.
func TestLocalAccessStopsWhenTheLimiterRefuses(t *testing.T) {
	local, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	local.SetLocalIOLimiter(&countingLocalIO{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := local.Write(ctx, "file", bytes.NewReader([]byte("x")), WriteOptions{Size: 1, Overwrite: true}); err == nil {
		t.Fatal("a write went ahead without local budget")
	}
}
