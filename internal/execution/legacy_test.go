package execution

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

type legacyObservedStore struct {
	transferstore.Store
	writes    int
	afterRead func()
}

func (s *legacyObservedStore) Write(ctx context.Context, key string, r io.Reader, opts transferstore.WriteOptions) (transferstore.WriteResult, error) {
	s.writes++
	return s.Store.Write(ctx, key, r, opts)
}
func (s *legacyObservedStore) Open(ctx context.Context, key string, opts transferstore.OpenOptions) (io.ReadCloser, transferstore.Object, error) {
	r, o, e := s.Store.Open(ctx, key, opts)
	if e != nil {
		return r, o, e
	}
	return &legacyObservedReader{ReadCloser: r, afterRead: s.afterRead}, o, nil
}

type legacyObservedReader struct {
	io.ReadCloser
	afterRead func()
}

func (r *legacyObservedReader) Read(p []byte) (int, error) {
	n, e := r.ReadCloser.Read(p)
	if e == io.EOF && r.afterRead != nil {
		r.afterRead()
		r.afterRead = nil
	}
	return n, e
}

func TestLegacyReconciliationReadsBodiesAndNeverCreditsMissingOrChangedSource(t *testing.T) {
	for _, scenario := range []string{"equal", "different-overwrite", "different-skip", "source-disappeared", "source-mutated-during-target-read"} {
		t.Run(scenario, func(t *testing.T) {
			src, dst := t.TempDir(), t.TempDir()
			data := bytes.Repeat([]byte("source"), 24000)
			if err := os.WriteFile(filepath.Join(src, "file"), data, 0600); err != nil {
				t.Fatal(err)
			}
			targetData := data
			if strings.HasPrefix(scenario, "different") {
				targetData = bytes.Repeat([]byte("wrong!"), 24000)
			}
			if err := os.WriteFile(filepath.Join(dst, "file"), targetData, 0600); err != nil {
				t.Fatal(err)
			}
			source, err := transferstore.NewLocal(src)
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			target, err := transferstore.NewLocal(dst)
			if err != nil {
				t.Fatal(err)
			}
			defer target.Close()
			observed := &legacyObservedStore{Store: target}
			obj, err := source.Stat(bg, "file")
			if err != nil {
				t.Fatal(err)
			}
			job := Job{ConflictPolicy: "OVERWRITE"}
			if scenario == "different-skip" {
				job.ConflictPolicy = "SKIP"
			}
			item := Item{ID: "file", Source: obj, DestinationKey: "file", Legacy: &LegacyCandidate{Successful: true}}
			engine := &Engine{}
			if scenario == "source-disappeared" {
				if err := os.Remove(filepath.Join(src, "file")); err != nil {
					t.Fatal(err)
				}
				job.ConflictPolicy = "SKIP"
			}
			if scenario == "source-mutated-during-target-read" {
				observed.afterRead = func() {
					if err := os.WriteFile(filepath.Join(src, "file"), bytes.Repeat([]byte("change"), 24000), 0600); err != nil {
						t.Error(err)
					}
					future := time.Now().Add(time.Minute)
					if err := os.Chtimes(filepath.Join(src, "file"), future, future); err != nil {
						t.Error(err)
					}
				}
			}
			result := engine.copyItem(bg, job, item, source, observed)
			switch scenario {
			case "equal":
				if result.Status != ItemSucceeded || result.TargetCommitted || result.Reconciliation != "SHA256_VERIFIED" || observed.writes != 0 {
					t.Fatal(result, observed.writes)
				}
			case "different-overwrite":
				if result.Status != ItemSucceeded || !result.TargetCommitted || result.Reconciliation != "CONTENT_MISMATCH" || observed.writes != 1 {
					t.Fatal(result, observed.writes)
				}
				got, _ := os.ReadFile(filepath.Join(dst, "file"))
				if !bytes.Equal(got, data) {
					t.Fatal("copy did not replace corrupt target")
				}
			case "different-skip":
				if result.Status != ItemSkipped || result.Reconciliation != "CONTENT_MISMATCH" || observed.writes != 0 {
					t.Fatal(result, observed.writes)
				}
			default:
				if result.Status != ItemFailed || result.Error == "" || observed.writes != 0 {
					t.Fatal(result, observed.writes)
				}
			}
		})
	}
}
