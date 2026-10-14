package transferstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

func TestSharedProbeEnsureReplayDoesNotOverwrite(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	probe, err := NewSharedProbe()
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	errorsSeen := make(chan error, 8)
	for range 8 {
		group.Go(func() { errorsSeen <- EnsureSharedProbe(ctx, root, probe) })
	}
	group.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 1 || entries[0].Name() != probe.Key {
		t.Fatalf("replay created multiple markers: %v %v", entries, err)
	}
	if err := os.WriteFile(filepath.Join(root, probe.Key), []byte("existing content"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSharedProbe(ctx, root, probe); !errors.Is(err, ErrSharedProbe) {
		t.Fatalf("ensure overwrote existing content: %v", err)
	}
	if content, err := os.ReadFile(filepath.Join(root, probe.Key)); err != nil || string(content) != "existing content" {
		t.Fatal("existing contents damaged")
	}
}

func TestLocalDiscoveryHidesOnlyValidInternalSharedMarkers(t *testing.T) {
	ctx := context.Background()
	store, root := localFixture(t)
	if _, err := CreateSharedProbe(ctx, root); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateSharedProbe(ctx, filepath.Join(root, "nested")); err != nil {
		t.Fatal(err)
	}
	similar, err := NewSharedProbe()
	if err != nil {
		t.Fatal(err)
	}
	validContents, _ := probeContents(similar)
	userObjects := map[string][]byte{
		"ordinary":                            []byte("user content"),
		sharedProbePrefix + "user-file.probe": validContents,
		similar.Key:                           []byte(strings.Repeat("x", len(validContents))),
	}
	want := []string{}
	for key, contents := range userObjects {
		writeLocal(t, store, key, contents)
		want = append(want, key)
	}
	sort.Strings(want)
	var listed, scanned []string
	options := ListOptions{Limit: 1}
	for {
		page, err := store.List(ctx, options)
		if err != nil {
			t.Fatal(err)
		}
		for _, object := range page.Objects {
			listed = append(listed, object.Key)
		}
		if page.NextCursor == "" {
			break
		}
		options.Cursor = page.NextCursor
	}
	if err := store.Scan(ctx, ListOptions{}, func(o Object) error { scanned = append(scanned, o.Key); return nil }); err != nil {
		t.Fatal(err)
	}
	sort.Strings(listed)
	sort.Strings(scanned)
	if !reflect.DeepEqual(listed, want) || !reflect.DeepEqual(scanned, want) {
		t.Fatalf("probe discovery filter affected user objects: list=%v scan=%v want=%v", listed, scanned, want)
	}
}

func TestSharedProbeSameRootSeparateRootAndCleanup(t *testing.T) {
	ctx := context.Background()
	root, other := t.TempDir(), t.TempDir()
	probe, err := CreateSharedProbe(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifySharedProbe(ctx, filepath.Join(root, "."), probe); err != nil {
		t.Fatalf("same root failed proof: %v", err)
	}
	if err := VerifySharedProbe(ctx, other, probe); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("different root accepted: %v", err)
	}
	wrong := probe
	wrong.Nonce = strings.Repeat("0", 64)
	if err := VerifySharedProbe(ctx, root, wrong); !errors.Is(err, ErrSharedProbe) {
		t.Fatalf("wrong nonce accepted: %v", err)
	}
	if err := CleanupSharedProbe(ctx, root, wrong); !errors.Is(err, ErrSharedProbe) {
		t.Fatalf("wrong nonce deleted proof: %v", err)
	}
	if err := VerifySharedProbe(ctx, root, probe); err != nil {
		t.Fatalf("invalid cleanup damaged proof: %v", err)
	}
	if err := CleanupSharedProbe(ctx, root, probe); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, probe.Key)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marker still exists: %v", err)
	}
	if err := CleanupSharedProbe(ctx, root, probe); err != nil {
		t.Fatalf("cleanup not idempotent: %v", err)
	}
}

func TestSharedProbeRefusesArbitraryDeleteAndSymlink(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	probe, err := CreateSharedProbe(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"ordinary", "../" + probe.Key, "sub/" + probe.Key, probe.Key + "/", strings.ToUpper(probe.Key)} {
		invalid := SharedProbe{Key: key, Nonce: probe.Nonce}
		if err := CleanupSharedProbe(ctx, root, invalid); !errors.Is(err, ErrSharedProbe) {
			t.Errorf("accepted cleanup key %q: %v", key, err)
		}
	}
	contents, err := os.ReadFile(filepath.Join(root, probe.Key))
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, contents, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, probe.Key)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, probe.Key)); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := VerifySharedProbe(ctx, root, probe); !errors.Is(err, ErrSharedProbe) {
		t.Fatalf("symlink accepted: %v", err)
	}
	if err := CleanupSharedProbe(ctx, root, probe); !errors.Is(err, ErrSharedProbe) {
		t.Fatalf("symlink cleanup accepted: %v", err)
	}
	if actual, err := os.ReadFile(outside); err != nil || string(actual) != string(contents) {
		t.Fatal("outside content modified")
	}
}

func TestSharedProbeCancellationAndChangedContents(t *testing.T) {
	root := t.TempDir()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CreateSharedProbe(canceled, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled creation: %v", err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("canceled creation left files: %v %v", entries, err)
	}
	probe, err := CreateSharedProbe(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := CleanupSharedProbe(canceled, root, probe); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled cleanup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, probe.Key), []byte("changed ordinary content"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := CleanupSharedProbe(context.Background(), root, probe); !errors.Is(err, ErrSharedProbe) {
		t.Fatalf("cleanup deleted replaced content: %v", err)
	}
	if contents, err := os.ReadFile(filepath.Join(root, probe.Key)); err != nil || string(contents) != "changed ordinary content" {
		t.Fatal("changed content was removed")
	}
}
