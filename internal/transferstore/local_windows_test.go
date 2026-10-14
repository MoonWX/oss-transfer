//go:build windows

package transferstore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsNamesDoNotAliasOrOpenStreams(t *testing.T) {
	for _, key := range []string{"file:stream", "trailing.", "trailing ", "dir/CON", "aux.txt", "LPT1", "COM8.log", "a|b", "nul", "a/CONOUT$"} {
		if _, err := localKey(key, false); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("accepted Windows alias %q: %v", key, err)
		}
	}
}

func TestWindowsIdentitySurvivesIdenticalSizeAndTimeReplacement(t *testing.T) {
	s, root := localFixture(t)
	ctx := context.Background()
	writeLocal(t, s, "target", []byte("first"))
	before, err := s.Stat(ctx, "target")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(filepath.Join(root, "target"), filepath.Join(root, "previous")); err != nil {
		t.Fatal(err)
	}
	writeLocal(t, s, "target", []byte("other"))
	if err = os.Chtimes(filepath.Join(root, "target"), before.ModifiedAt, before.ModifiedAt); err != nil {
		t.Fatal(err)
	}
	after, err := s.Stat(ctx, "target")
	if err != nil {
		t.Fatal(err)
	}
	if before.ETag == after.ETag {
		t.Fatal("fingerprint ignored Windows file ID")
	}
	if _, _, err = s.Open(ctx, "target", OpenOptions{Expected: &before}); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("accepted replaced source %v", err)
	}
}

func TestWindowsAtomicReplacementAndFinalFlush(t *testing.T) {
	s, root := localFixture(t)
	writeLocal(t, s, "target", []byte("old"))
	payload := bytes.Repeat([]byte("new"), 1000)
	writeLocal(t, s, "target", payload)
	data, err := os.ReadFile(filepath.Join(root, "target"))
	if err != nil || !bytes.Equal(data, payload) {
		t.Fatalf("replace/flush failed: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary link remained %v", err)
	}
}
