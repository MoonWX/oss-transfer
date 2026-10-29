package servicehost

import (
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ScanReleasePermission keeps remote scanner copies available until a managed
// installation has left its database rollback window. Older installers do not
// write a commit marker; transfers still work, but their scan copies are kept.
func ScanReleasePermission(readyFile string) func() bool {
	if readyFile == "" {
		return func() bool { return true }
	}
	request, ok := readLaunchNonce(readyFile + ".request")
	if !ok {
		return func() bool { return false }
	}
	var committed atomic.Bool
	var mu sync.Mutex
	var checkedAt time.Time
	return func() bool {
		if committed.Load() {
			return true
		}
		mu.Lock()
		defer mu.Unlock()
		if committed.Load() {
			return true
		}
		if time.Since(checkedAt) < time.Second {
			return false
		}
		checkedAt = time.Now()
		currentRequest, ok := readLaunchNonce(readyFile + ".request")
		if !ok || currentRequest != request {
			return false
		}
		confirmation, ok := readLaunchNonce(readyFile + ".committed")
		if !ok || confirmation != request {
			return false
		}
		committed.Store(true)
		return true
	}
}

func readLaunchNonce(path string) (string, bool) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	file, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return "", false
	}
	raw, err := io.ReadAll(io.LimitReader(file, 128))
	nonce := strings.TrimSpace(string(raw))
	if err != nil || len(raw) == 128 || !noncePattern.MatchString(nonce) {
		return "", false
	}
	return nonce, true
}
