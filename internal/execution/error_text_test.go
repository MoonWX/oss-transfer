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

// The cause of an object failure is matched without its key, which can hold
// any word: a quote in the key is escaped, so the key ends at the first
// unescaped quote.
func TestObjectErrorCauseLeavesOutTheKey(t *testing.T) {
	for _, test := range []struct{ text, want string }{
		{fmt.Sprintf("write destination %q: request send failed: EOF", `logs/EOF": connection reset`), "request send failed: EOF"},
		{`no space left on device: write destination "medium/m1.bin": write /files/m1.tmp: no space left on device`, "no space left on device: write /files/m1.tmp: no space left on device"},
		{`verify write "videofiles/a.mp4": destination did not confirm the complete object`, "destination did not confirm the complete object"},
		{"object execution panic: boom", "object execution panic: boom"},
	} {
		if got := ObjectErrorCause(test.text); got != test.want {
			t.Errorf("%s: got %q, want %q", test.text, got, test.want)
		}
	}
}
