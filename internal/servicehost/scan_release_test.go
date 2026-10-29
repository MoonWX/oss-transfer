package servicehost

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestScanReleasePermissionRequiresCommittedInstallation(t *testing.T) {
	if !ScanReleasePermission("")() {
		t.Fatal("unmanaged startup unexpectedly blocked scan release")
	}
	path := filepath.Join(t.TempDir(), "ready")
	write := func(suffix, nonce string) {
		t.Helper()
		if err := os.WriteFile(path+suffix, []byte(nonce+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if ScanReleasePermission(path)() {
		t.Fatal("missing installer markers granted release")
	}
	write(".request", strings.Repeat("a", 64))
	if ScanReleasePermission(path)() {
		t.Fatal("readiness alone granted irreversible cleanup")
	}
	write(".committed", strings.Repeat("b", 64))
	permission := ScanReleasePermission(path)
	if permission() {
		t.Fatal("a previous installation's commit granted release")
	}
	write(".committed", strings.Repeat("a", 64))
	deadline := time.Now().Add(2 * time.Second)
	for !permission() {
		if time.Now().After(deadline) {
			t.Fatal("running candidate did not observe installer commit")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var callers sync.WaitGroup
	for range 8 {
		callers.Add(1)
		go func() {
			defer callers.Done()
			if !permission() {
				t.Error("committed installation did not permit release")
			}
		}()
	}
	callers.Wait()
	if !ScanReleasePermission(path)() {
		t.Fatal("ordinary service restart lost its committed installation")
	}
	write(".request", strings.Repeat("c", 64))
	if ScanReleasePermission(path)() {
		t.Fatal("a new candidate reused the previous release permission")
	}
}

func TestScanReleasePermissionNeverAdoptsAnotherStartup(t *testing.T) {
	for _, committed := range []string{strings.Repeat("a", 64), strings.Repeat("b", 64)} {
		t.Run(committed[:1], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ready")
			if err := os.WriteFile(path+".request", []byte(strings.Repeat("a", 64)), 0600); err != nil {
				t.Fatal(err)
			}
			permission := ScanReleasePermission(path)
			if err := os.WriteFile(path+".request", []byte(strings.Repeat("b", 64)), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path+".committed", []byte(committed), 0600); err != nil {
				t.Fatal(err)
			}
			if permission() {
				t.Fatal("candidate adopted another launch's commit")
			}
		})
	}
}

func TestScanReleasePermissionRejectsUnsafeMarkers(t *testing.T) {
	for _, suffix := range []string{".request", ".committed"} {
		for _, kind := range []string{"missing", "empty", "short", "nonhex", "oversized", "directory", "symlink"} {
			t.Run(suffix+"/"+kind, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "ready")
				nonce := strings.Repeat("a", 64)
				for _, extension := range []string{".request", ".committed"} {
					if err := os.WriteFile(path+extension, []byte(nonce+"\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				bad := path + suffix
				if err := os.Remove(bad); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "missing":
				case "directory":
					if err := os.Mkdir(bad, 0700); err != nil {
						t.Fatal(err)
					}
				case "symlink":
					target := path + ".target"
					if err := os.WriteFile(target, []byte(nonce), 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(target, bad); err != nil {
						t.Skipf("symlinks unavailable: %v", err)
					}
				default:
					value := map[string]string{"empty": "", "short": "a", "nonhex": strings.Repeat("g", 64), "oversized": nonce + strings.Repeat(" ", 128)}[kind]
					if err := os.WriteFile(bad, []byte(value), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if ScanReleasePermission(path)() {
					t.Fatal("unsafe startup marker granted scan release")
				}
			})
		}
	}
}
