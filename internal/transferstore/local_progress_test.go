package transferstore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// A local write counts each byte once it is in the temporary file, so the
// object holds every byte before it is synced and published. Bytes the
// source has not handed over yet are not counted.
func TestLocalWriteCountsBytesInTheTemporaryFile(t *testing.T) {
	for _, ranged := range []bool{false, true} {
		t.Run(fmt.Sprintf("ranged=%v", ranged), func(t *testing.T) {
			size := 9*256<<10 + 123
			data := payload(size)
			root := t.TempDir()
			store, err := NewLocal(root)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			memory, _ := NewUploadMemoryBudget(64 << 20)
			if err := store.SetRangedWrites(LocalRangedOptions{MinSize: 1, ChunkSize: 256 << 10, Memory: memory}); err != nil {
				t.Fatal(err)
			}
			progress := &recordingProgress{limit: int64(size)}
			options := WriteOptions{Size: int64(size), Overwrite: true, Progress: progress}
			var input io.Reader = bytes.NewReader(data)
			half, checked := make(chan struct{}), make(chan struct{})
			if ranged {
				options.Ranges = rangesOf(data)
				close(half)
			} else {
				reader, writer := io.Pipe()
				input = reader
				go func() {
					_, _ = writer.Write(data[:size/2])
					close(half)
					<-checked
					_, _ = writer.Write(data[size/2:])
					_ = writer.Close()
				}()
			}
			done := make(chan error, 1)
			go func() {
				result, err := store.Write(context.Background(), "dir/object", input, options)
				if err == nil && !result.Committed {
					err = fmt.Errorf("not committed: %+v", result)
				}
				done <- err
			}()
			<-half
			if _, accepted, _, _ := progress.state(); !ranged && accepted > int64(size)/2 {
				t.Errorf("counted %d bytes the source had not handed over", accepted)
			}
			close(checked)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if got, err := os.ReadFile(filepath.Join(root, "dir", "object")); err != nil || !bytes.Equal(got, data) {
				t.Fatalf("published file differs: %v", err)
			}
			progress.check(t, int64(size), int64(size))
		})
	}
}
