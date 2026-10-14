package transferstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type memoryRecovery struct {
	mu              sync.Mutex
	files           map[string]LocalTempRecord
	uploads         map[string]S3UploadRecord
	rejectUpload    bool
	rejectSpool     bool
	registered      int
	spoolRegistered int
}

func newMemoryRecovery() *memoryRecovery {
	return &memoryRecovery{files: map[string]LocalTempRecord{}, uploads: map[string]S3UploadRecord{}}
}
func (r *memoryRecovery) OwnerID() string { return strings.Repeat("a", 64) }
func (r *memoryRecovery) Register(_ context.Context, record LocalTempRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rejectSpool {
		return errors.New("spool journal unavailable")
	}
	if err := ValidateLocalTempRecord(record); err != nil {
		return err
	}
	r.spoolRegistered++
	r.files[record.ID] = record
	return nil
}
func (r *memoryRecovery) Forget(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.files, id)
	return nil
}
func (r *memoryRecovery) RegisterUpload(_ context.Context, record S3UploadRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rejectUpload {
		return errors.New("upload journal unavailable")
	}
	if err := ValidateS3UploadRecord(record); err != nil {
		return err
	}
	r.registered++
	r.uploads[record.ID] = record
	return nil
}
func (r *memoryRecovery) ForgetUpload(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.uploads, id)
	return nil
}

func TestS3RecoveryRegistersBeforePayloadAndClearsCompletedOrAbortedWork(t *testing.T) {
	for _, mode := range []string{"complete", "short", "reject-upload", "reject-spool"} {
		t.Run(mode, func(t *testing.T) {
			p := newProtocolS3(t)
			s := p.store(t)
			s.maxBufferedBytes = 1024 * 1024
			s.spoolDirectory = filepath.Join(t.TempDir(), "new-spool")
			r := newMemoryRecovery()
			r.rejectUpload, r.rejectSpool = mode == "reject-upload", mode == "reject-spool"
			if err := s.SetRecoveryRegistry(r, "configured", r); err != nil {
				t.Fatal(err)
			}
			p.onPart = func(_ int) {
				r.mu.Lock()
				defer r.mu.Unlock()
				if len(r.uploads) != 1 || len(r.files) != 1 {
					t.Errorf("payload sent before upload/spool registration: %d/%d", len(r.uploads), len(r.files))
				}
				for _, file := range r.files {
					if info, err := os.Stat(filepath.Join(file.ParentPath, file.Name)); err != nil || info.Size() <= 0 {
						t.Errorf("tracked part missing: %v", err)
					}
				}
			}
			payload := bytes.Repeat([]byte("multipart bytes"), 450000)
			var reader io.Reader = bytes.NewReader(payload)
			if mode == "short" {
				reader = strings.NewReader("partial input")
			} else if r.rejectUpload || r.rejectSpool {
				reader = panicReader{}
			}
			result, err := s.Write(context.Background(), "tracked", reader, WriteOptions{Size: int64(len(payload)), Overwrite: true})
			if mode == "complete" {
				if err != nil || !result.Committed || p.completed != 1 || !bytes.Equal(p.objects["tracked"].data, payload) {
					t.Fatalf("completion failed: %+v %v", result, err)
				}
			} else if err == nil || result.Committed || p.aborted != 1 || p.completed != 0 {
				t.Fatalf("failed write was not aborted: %+v %v", result, err)
			}
			if len(r.files) != 0 || len(r.uploads) != 0 {
				t.Fatalf("finished work left recovery records: %d/%d", len(r.files), len(r.uploads))
			}
			files, listErr := os.ReadDir(s.spoolDirectory)
			if !errors.Is(listErr, os.ErrNotExist) && (listErr != nil || len(files) != 0) {
				t.Fatalf("part spool not cleaned: %v %v", files, listErr)
			}
			if mode == "complete" && (r.registered != 1 || r.spoolRegistered != 2) {
				t.Fatalf("expected durable barriers for upload and both parts: %d/%d", r.registered, r.spoolRegistered)
			}
		})
	}
}

func TestS3RecoveryCanceledUploadRetainsRecordUntilAbortSucceeds(t *testing.T) {
	for _, denied := range []bool{false, true} {
		name := "abort-succeeds"
		if denied {
			name = "abort-denied"
		}
		t.Run(name, func(t *testing.T) {
			p := newProtocolS3(t)
			p.abortDenied = denied
			s := p.store(t)
			s.maxBufferedBytes = 1024 * 1024
			s.spoolDirectory = t.TempDir()
			r := newMemoryRecovery()
			if err := s.SetRecoveryRegistry(r, "configured", r); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p.onPart = func(_ int) { cancel() }
			payload := bytes.Repeat([]byte("cancel during upload"), 350000)
			result, err := s.Write(ctx, "canceled", bytes.NewReader(payload), WriteOptions{Size: int64(len(payload)), Overwrite: true})
			if err == nil || !errors.Is(err, context.Canceled) || result.Committed {
				t.Fatalf("canceled upload reported success: %+v %v", result, err)
			}
			if len(r.files) != 0 || len(r.uploads) != map[bool]int{false: 0, true: 1}[denied] {
				t.Fatalf("incorrect recovery state after cancel: %d spool / %d MPU", len(r.files), len(r.uploads))
			}
			if files, err := os.ReadDir(s.spoolDirectory); err != nil || len(files) != 0 {
				t.Fatalf("cancellation left spool files: %v %v", files, err)
			}
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.partAttempts[1] != 1 || p.aborted != 1 || p.completed != 0 {
				t.Fatal("test did not cancel after a real protocol part attempt")
			}
		})
	}
}
