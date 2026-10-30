package transferstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestLocalCheckpointScanReopensEveryPageWithoutDuplicates(t *testing.T) {
	local, directory := localFixture(t)
	for _, key := range []string{"a/0", "a/deep/1", "a/deep/2", "a.txt", "b/0", "b.txt", "z/last"} {
		writeLocal(t, local, key, []byte(key))
	}
	if err := os.Mkdir(filepath.Join(directory, "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []ListOptions{{}, {Prefix: "a"}, {StartAfter: "a.txt"}, {Prefix: "absent"}} {
		t.Run(fmt.Sprintf("prefix=%s,after=%s", scope.Prefix, scope.StartAfter), func(t *testing.T) {
			var want, got []Object
			if err := local.Scan(context.Background(), scope, func(object Object) error { want = append(want, object); return nil }); err != nil {
				t.Fatal(err)
			}
			options := scope
			options.Limit = 2
			stop := errors.New("process stopped after saving the page")
			for attempts := 0; ; attempts++ {
				if attempts > 30 {
					t.Fatal("cursor made no progress")
				}
				reopened, err := NewLocal(directory)
				if err != nil {
					t.Fatal(err)
				}
				calls := 0
				err = reopened.ScanPages(context.Background(), options, func(page Page) error {
					calls++
					got = append(got, page.Objects...)
					options.Cursor = page.NextCursor
					return stop
				})
				_ = reopened.Close()
				if !errors.Is(err, stop) || calls != 1 {
					t.Fatalf("callback backpressure ignored: calls=%d err=%v", calls, err)
				}
				if options.Cursor == "" {
					break
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("resume differs from full discovery: got=%+v want=%+v", got, want)
			}
		})
	}
}

func TestLocalCheckpointSkipsCompletedSubtrees(t *testing.T) {
	local, directory := localFixture(t)
	writeLocal(t, local, "a/complete", []byte("before"))
	writeLocal(t, local, "b/pending", []byte("after"))
	stop := errors.New("saved")
	var checkpoint string
	err := local.ScanPages(context.Background(), ListOptions{Limit: 2}, func(page Page) error {
		if len(page.Objects) != 1 || page.Objects[0].Key != "a/complete" {
			t.Fatalf("unexpected first page: %+v", page)
		}
		checkpoint = page.NextCursor
		return stop
	})
	if !errors.Is(err, stop) || checkpoint == "" {
		t.Fatalf("no checkpoint: %v", err)
	}
	// Changes behind the checkpoint are intentionally not an incremental scan.
	// More importantly, the old completed subtree may no longer be readable.
	writeLocal(t, local, "a/new-after-checkpoint", []byte("not part of the remaining scan"))
	if runtime.GOOS != "windows" {
		if err := os.Chmod(filepath.Join(directory, "a"), 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(directory, "a"), 0700) })
	}
	reopened, err := NewLocal(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var got []string
	err = reopened.ScanPages(context.Background(), ListOptions{Cursor: checkpoint}, func(page Page) error {
		for _, object := range page.Objects {
			got = append(got, object.Key)
		}
		return nil
	})
	if err != nil || !reflect.DeepEqual(got, []string{"b/pending"}) {
		t.Fatalf("completed subtree was visited: %v %v", got, err)
	}
}

func TestLocalCheckpointDetectsReplacedActiveDirectory(t *testing.T) {
	local, directory := localFixture(t)
	writeLocal(t, local, "active/1", []byte("one"))
	writeLocal(t, local, "active/2", []byte("two"))
	var checkpoint string
	stop := errors.New("stop")
	err := local.ScanPages(context.Background(), ListOptions{Limit: 2}, func(page Page) error {
		checkpoint = page.NextCursor
		return stop
	})
	if !errors.Is(err, stop) || checkpoint == "" {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(directory, "active"), filepath.Join(directory, "old")); err != nil {
		t.Fatal(err)
	}
	for _, replacement := range []bool{false, true} {
		if replacement {
			if err := os.Mkdir(filepath.Join(directory, "active"), 0700); err != nil {
				t.Fatal(err)
			}
		}
		calls := 0
		err := local.ScanPages(context.Background(), ListOptions{Cursor: checkpoint}, func(Page) error { calls++; return nil })
		if !errors.Is(err, ErrSourceChanged) || calls != 0 {
			t.Fatalf("replaced active directory accepted: replacement=%v calls=%d err=%v", replacement, calls, err)
		}
	}
}

func TestLocalCheckpointRejectsWrongScopeAndMalformedStacks(t *testing.T) {
	local, _ := localFixture(t)
	writeLocal(t, local, "a", []byte("one"))
	writeLocal(t, local, "b", []byte("two"))
	var checkpoint string
	stop := errors.New("stop")
	_ = local.ScanPages(context.Background(), ListOptions{Limit: 1}, func(page Page) error { checkpoint = page.NextCursor; return stop })
	for _, options := range []ListOptions{{Prefix: "a", Cursor: checkpoint}, {StartAfter: "a", Cursor: checkpoint}, {Cursor: "not-a-cursor"}} {
		if err := local.ScanPages(context.Background(), options, func(Page) error { t.Fatal("invalid cursor emitted a page"); return nil }); !errors.Is(err, ErrInvalidCursor) {
			t.Fatalf("wrong scope accepted: %+v %v", options, err)
		}
	}
	other, _ := localFixture(t)
	if err := other.ScanPages(context.Background(), ListOptions{Cursor: checkpoint}, func(Page) error { return nil }); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("another root accepted the cursor: %v", err)
	}
	oldPage, err := local.List(context.Background(), ListOptions{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := local.ScanPages(context.Background(), ListOptions{Cursor: oldPage.NextCursor}, func(Page) error { return nil }); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("interactive listing cursor accepted: %v", err)
	}
	for _, frames := range [][]localScanFrame{nil, {{Name: "../escape", Identity: "x"}}, {{Identity: "x", After: ".."}}, {{Identity: "x", After: "a"}, {Name: "other", Identity: "y"}}} {
		raw, _ := json.Marshal(frames)
		cursor := makeCursor(localScanCursorKind, local.scope, "", "", string(raw))
		if err := local.ScanPages(context.Background(), ListOptions{Cursor: cursor}, func(Page) error { return nil }); !errors.Is(err, ErrInvalidCursor) {
			t.Fatalf("malformed frames accepted: %+v %v", frames, err)
		}
	}
}

func TestLocalCheckpointEmptyPagesAdvanceAndCancellationStops(t *testing.T) {
	local, directory := localFixture(t)
	for i := range 9 {
		if err := os.Mkdir(filepath.Join(directory, fmt.Sprintf("%02d", i)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	var checkpoints []string
	err := local.ScanPages(context.Background(), ListOptions{Limit: 2}, func(page Page) error {
		if len(page.Objects) != 0 {
			t.Fatal("empty directory produced objects")
		}
		checkpoints = append(checkpoints, page.NextCursor)
		return nil
	})
	if err != nil || len(checkpoints) != 5 || checkpoints[4] != "" {
		t.Fatalf("empty scan lost progress: %v %v", checkpoints, err)
	}
	for i := 0; i < len(checkpoints)-1; i++ {
		if checkpoints[i] == "" || i > 0 && checkpoints[i] == checkpoints[i-1] {
			t.Fatal("nonterminal empty page did not advance")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err = local.ScanPages(ctx, ListOptions{Limit: 2}, func(Page) error { calls++; cancel(); return nil })
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("cancellation ignored: calls=%d %v", calls, err)
	}
}

func TestLocalCheckpointExcludesInternalFilesAndSymlinks(t *testing.T) {
	directory, _, namespace := makeTestNamespace(t)
	if err := os.WriteFile(filepath.Join(directory, namespace.Name, "partial"), []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateSharedProbe(context.Background(), directory); err != nil {
		t.Fatal(err)
	}
	local, err := NewLocal(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	writeLocal(t, local, "visible", []byte("public"))
	if runtime.GOOS != "windows" {
		if err := os.Symlink(t.TempDir(), filepath.Join(directory, "outside")); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	err = local.ScanPages(context.Background(), ListOptions{Limit: 1}, func(page Page) error {
		for _, object := range page.Objects {
			got = append(got, object.Key)
		}
		return nil
	})
	if err != nil || strings.Join(got, ",") != "visible" {
		t.Fatalf("internal files or links included: %v %v", got, err)
	}
}

func TestLocalCheckpointDirectoryReplacementCannotEscapeRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a directory symlink needs an elevated Windows token")
	}
	local, directory := localFixture(t)
	writeLocal(t, local, "active/1", []byte("one"))
	writeLocal(t, local, "active/2", []byte("two"))
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "2"), []byte("outside data"), 0600); err != nil {
		t.Fatal(err)
	}
	var got []string
	err := local.ScanPages(context.Background(), ListOptions{Limit: 2}, func(page Page) error {
		for _, object := range page.Objects {
			got = append(got, object.Key)
		}
		if len(got) != 1 {
			t.Fatalf("outside file was scanned after directory substitution: %v", got)
		}
		if err := os.Rename(filepath.Join(directory, "active"), filepath.Join(directory, "old")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(directory, "active")); err != nil {
			t.Fatal(err)
		}
		return nil
	})
	if err == nil || !reflect.DeepEqual(got, []string{"active/1"}) {
		t.Fatalf("directory substitution escaped root: got=%v err=%v", got, err)
	}
}

func TestLocalCheckpointKeepsDirectoryCursorAcrossNameChanges(t *testing.T) {
	local, directory := localFixture(t)
	writeLocal(t, local, "active/1", []byte("one"))
	writeLocal(t, local, "active/2", []byte("two"))
	var checkpoint string
	stop := errors.New("stop")
	if err := local.ScanPages(context.Background(), ListOptions{Limit: 2}, func(page Page) error { checkpoint = page.NextCursor; return stop }); !errors.Is(err, stop) {
		t.Fatal(err)
	}
	writeLocal(t, local, "active/0", []byte("behind cursor"))
	writeLocal(t, local, "active/3", []byte("ahead of cursor"))
	reopened, err := NewLocal(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var got []string
	err = reopened.ScanPages(context.Background(), ListOptions{Cursor: checkpoint}, func(page Page) error {
		for _, object := range page.Objects {
			got = append(got, object.Key)
		}
		return nil
	})
	if err != nil || !reflect.DeepEqual(got, []string{"active/2", "active/3"}) {
		t.Fatalf("directory mtime invalidated resumable cursor: %v %v", got, err)
	}
}
