//go:build !linux && !darwin

package legacyimport

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/state"
)

// This boundary test also runs on Windows, where preparing a POSIX migration
// fixture is intentionally unsupported. Nil DB handles prove no DB is accessed.
func TestFinalizeUnsupportedPlatformRefusesBeforeAccessingPreparedState(t *testing.T) {
	root := t.TempDir()
	stage := filepath.Join(root, "staging")
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	p := &Prepared{Directory: stage, Manifest: PreparationManifest{
		MigrationID: "refusal-test", Pending: true, Destination: filepath.Join(root, "published"),
	}}
	if err := writePrivateJSON(filepath.Join(stage, "manifest.json"), p.Manifest); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "original-data"), []byte("retain original bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	before := snapshotPublicationFiles(t, root)
	if err := p.Finalize(context.Background()); !errors.Is(err, errUnsupportedPublication) {
		t.Fatalf("expected explicit platform refusal, got %v", err)
	}
	if p.Directory != stage || !p.Manifest.Pending || p.closed {
		t.Fatal("refusal changed preparation state")
	}
	if !reflect.DeepEqual(before, snapshotPublicationFiles(t, root)) {
		t.Fatal("unsupported finalization changed files or published a destination")
	}
}

func TestFinalizeUnsupportedPlatformPreservesStagingRawDataAndStartupFences(t *testing.T) {
	// prepareFixture retains its existing Windows skip; FreeBSD runs the full
	// conversion so rejection is checked against genuine prepared databases.
	p := finalizedFixture(t)
	root := filepath.Dir(p.Directory)
	directory := p.Directory
	manifest, err := json.Marshal(p.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	stores := map[string]*state.Store{"master": p.Master}
	for name, db := range p.Agents {
		stores["agent:"+name] = db
	}
	checkFences := func() {
		for name, db := range stores {
			role := "agent"
			if name == "master" {
				role = "master"
			}
			if err := verifyPending(db, p.Manifest.MigrationID); err != nil {
				t.Fatalf("%s lost its pending record: %v", name, err)
			}
			if err := db.ClaimRole(role); !errors.Is(err, state.ErrMigrationPending) {
				t.Fatalf("%s became runnable: %v", name, err)
			}
		}
	}
	checkFences()
	before := snapshotPublicationFiles(t, root)
	if err := p.Finalize(context.Background()); !errors.Is(err, errUnsupportedPublication) {
		t.Fatalf("expected explicit platform refusal, got %v", err)
	}
	afterManifest, err := json.Marshal(p.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if p.Directory != directory || p.closed || string(manifest) != string(afterManifest) {
		t.Fatal("refusal altered or closed the preparation")
	}
	if !reflect.DeepEqual(before, snapshotPublicationFiles(t, root)) {
		t.Fatal("refusal changed staging, original snapshots, manifest, or database bytes")
	}
	if _, err := os.Lstat(p.Manifest.Destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsupported finalization published a destination: %v", err)
	}
	checkFences()
}

func snapshotPublicationFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		files[relative] = info.Mode().String()
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			hash := sha256.Sum256(data)
			files[relative] += string(hash[:])
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
