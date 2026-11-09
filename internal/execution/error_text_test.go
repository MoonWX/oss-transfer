package execution

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"syscall"
	"testing"
)

func TestObjectErrorsPutTheFileSystemCauseFirst(t *testing.T) {
	staging := "/files/medium/.oss-transfer-staging-" + strings.Repeat("a", 64) + "-" + strings.Repeat("b", 32) + "/.oss-transfer-" + strings.Repeat("a", 64) + "-" + strings.Repeat("c", 32) + ".tmp"
	full := fmt.Errorf("write destination %q: %w", "medium/m1.bin", &fs.PathError{Op: "write", Path: staging, Err: syscall.ENOSPC})
	got := objectErrorText(full)
	want := `no space left on device: write destination "medium/m1.bin": write /files/medium/.oss-transfer-staging-…/.oss-transfer-….tmp: no space left on device`
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	denied := fmt.Errorf("write destination %q: %w", "medium", &fs.PathError{Op: "mkdirat", Path: "medium", Err: syscall.EACCES})
	if got := objectErrorText(denied); !strings.HasPrefix(got, "permission denied: write destination") {
		t.Fatalf("permission error: %q", got)
	}
	// Other errors are unchanged.
	plain := errors.New(`open source "k": object not found`)
	if got := objectErrorText(plain); got != plain.Error() {
		t.Fatalf("plain error changed: %q", got)
	}
}
