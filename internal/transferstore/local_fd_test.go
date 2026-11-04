package transferstore

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"testing"
)

func openDescriptors(t *testing.T) int {
	t.Helper()
	for _, dir := range []string{"/proc/self/fd", "/dev/fd"} {
		if entries, err := os.ReadDir(dir); err == nil {
			return len(entries)
		}
	}
	t.Skip("no descriptor listing on this platform")
	return 0
}

// A Local writing into many directories keeps a bounded number of
// descriptors: idle directory entries are evicted (after syncing what they
// owe) and reservations hold no descriptor.
func TestLocalDescriptorsStayBoundedAcrossDirectories(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("descriptor listing is Unix-only")
	}
	root := t.TempDir()
	log := &eventLog{}
	hookSyncs(t, log, false)
	registry := newEventRegistry(log)
	local, err := NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	local.SetTempRegistry(registry)
	before := openDescriptors(t)
	peak := before
	for dir := range 200 {
		var keys []string
		for i := range 4 {
			keys = append(keys, fmt.Sprintf("d%03d/f%d", dir, i))
		}
		writeDeferred(t, local, keys, true)
		if dir%50 == 49 {
			if err := local.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		peak = max(peak, openDescriptors(t))
	}
	// Each cached entry holds at most four descriptors.
	if limit := before + 4*maxCachedDirs + 16; peak > limit {
		t.Fatalf("descriptors grew from %d to %d (limit %d)", before, peak, limit)
	}
	// Every directory's changes are synced exactly once, by eviction or by a
	// barrier; records are cleared only by barriers (checked elsewhere).
	if n := log.count("dir "); n != 2*200 {
		t.Fatalf("directory syncs: got %d want %d", n, 2*200)
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	if after := openDescriptors(t); after > before {
		t.Fatalf("Close leaked descriptors: %d before, %d after", before, after)
	}
	if registry.pending() != 0 {
		t.Fatalf("records left after Close: %d", registry.pending())
	}
	if names := tempNames(t, root); len(names) != 0 {
		t.Fatalf("temporary files remain: %v", names)
	}
}
