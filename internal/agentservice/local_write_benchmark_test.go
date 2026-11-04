package agentservice

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// BenchmarkLocalSmallFileDownload drives the real tracked Local target the way
// the execution engine does for OSS->LOCAL: up to 32 jobs run at once, each
// opens its own Local, writes up to 128 objects with 4 workers into one
// directory, makes them durable and closes. The source is in memory.
//
// Set OSS_TRANSFER_BENCH_DIR to measure a specific filesystem (for example a
// Docker volume instead of a bind mount).
func BenchmarkLocalSmallFileDownload(b *testing.B) {
	const (
		jobs       = 32
		perJob     = 128
		workers    = 4
		size       = 4 << 10
		dirs       = 20
		filesPerOp = jobs * perJob
	)
	base := os.Getenv("OSS_TRANSFER_BENCH_DIR")
	if base == "" {
		base = b.TempDir()
	} else {
		var err error
		if base, err = os.MkdirTemp(base, "bench-"); err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { os.RemoveAll(base) })
	}
	registry, _ := openTempRegistry(b, filepath.Join(base, "agent.db"))
	if _, err := registry.Recover(context.Background()); err != nil {
		b.Fatal(err)
	}
	root := filepath.Join(base, "root")
	if err := os.Mkdir(root, 0o755); err != nil {
		b.Fatal(err)
	}
	payload := bytes.Repeat([]byte{0x5a}, size)
	b.SetBytes(int64(filesPerOp * size))
	b.ResetTimer()
	for op := 0; op < b.N; op++ {
		var group sync.WaitGroup
		errs := make(chan error, jobs)
		for job := 0; job < jobs; job++ {
			group.Add(1)
			go func(job int) {
				defer group.Done()
				errs <- benchmarkLocalJob(registry, root, fmt.Sprintf("op%d/d%02d", op, job%dirs), job, perJob, workers, payload)
			}(job)
		}
		group.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				b.Fatal(err)
			}
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(b.N*filesPerOp)/b.Elapsed().Seconds(), "files/s")
}

func benchmarkLocalJob(registry *LocalTempRegistry, root, dir string, job, count, workers int, payload []byte) error {
	local, err := transferstore.NewLocal(root)
	if err != nil {
		return err
	}
	local.SetTempRegistry(registry)
	ctx := context.Background()
	keys := make(chan string)
	var group sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			for key := range keys {
				result, err := local.Write(ctx, key, bytes.NewReader(payload), transferstore.WriteOptions{Size: int64(len(payload)), Overwrite: true, DeferDurability: true})
				if err == nil && !result.Committed {
					err = fmt.Errorf("%s was not committed", key)
				}
				if err != nil {
					mu.Lock()
					firstErr = err
					mu.Unlock()
				}
			}
		}()
	}
	for i := range count {
		keys <- fmt.Sprintf("%s/j%02d-%04d.bin", dir, job, i)
	}
	close(keys)
	group.Wait()
	if firstErr != nil {
		local.Close()
		return firstErr
	}
	if err := local.Sync(ctx); err != nil {
		local.Close()
		return err
	}
	return local.Close()
}
