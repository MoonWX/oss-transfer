package transferstore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func sharedImportFixture(t *testing.T) (*Local, string, string) {
	t.Helper()
	local, root := localFixture(t)
	for _, key := range []string{"a/0", "a/1", "a/2", "z/last"} {
		writeLocal(t, local, key, []byte(key))
	}
	var saved string
	stop := errors.New("checkpoint persisted")
	err := local.ScanPages(context.Background(), ListOptions{Limit: 2}, func(page Page) error { saved = page.NextCursor; return stop })
	if !errors.Is(err, stop) || saved == "" {
		t.Fatalf("save cursor: %v", err)
	}
	return local, root, saved
}

func TestSharedScanCheckpointRebindsMountPathAndDeviceOnly(t *testing.T) {
	_, root, saved := sharedImportFixture(t)
	alias := filepath.Join(t.TempDir(), "mounted")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("directory alias unavailable: %v", err)
	}
	target, err := NewLocal(alias)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	// A shared filesystem has the same inode IDs, while each mount has its
	// own local device number. Persist the source mount's different device.
	data, err := base64.RawURLEncoding.DecodeString(saved)
	if err != nil {
		t.Fatal(err)
	}
	var old cursor
	if err := json.Unmarshal(data, &old); err != nil {
		t.Fatal(err)
	}
	var frames []localScanFrame
	if err := json.Unmarshal([]byte(old.Position), &frames); err != nil {
		t.Fatal(err)
	}
	for i := range frames {
		parts := strings.Split(frames[i].Identity, "/")
		if len(parts) != 3 {
			t.Skip("platform does not use Unix directory identities")
		}
		parts[1] = "987654321"
		frames[i].Identity = strings.Join(parts, "/")
	}
	encoded, _ := json.Marshal(frames)
	old.Scope = root + frames[0].Identity
	old.Position = string(encoded)
	encoded, _ = json.Marshal(old)
	saved = base64.RawURLEncoding.EncodeToString(encoded)
	rebound, err := target.ImportScanCursor(ListOptions{Cursor: saved}, root)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := target.ScanPages(context.Background(), ListOptions{Cursor: rebound}, func(page Page) error {
		for _, object := range page.Objects {
			got = append(got, object.Key)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"a/1", "a/2", "z/last"}) {
		t.Fatalf("rebound checkpoint lost remaining work: %v", got)
	}
}

func TestSharedScanCheckpointRejectsUnrelatedOrReplacedDirectories(t *testing.T) {
	source, root, saved := sharedImportFixture(t)
	other, _, _ := sharedImportFixture(t)
	if _, err := other.ImportScanCursor(ListOptions{Cursor: saved}, root); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("unrelated same-shaped root accepted: %v", err)
	}
	if _, err := source.ImportScanCursor(ListOptions{Cursor: saved}, filepath.Join(root, "wrong")); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("wrong source scope accepted: %v", err)
	}
	if err := os.Rename(filepath.Join(root, "a"), filepath.Join(root, "old-a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "a"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := source.ImportScanCursor(ListOptions{Cursor: saved}, root); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("replaced active child directory accepted: %v", err)
	}
}
