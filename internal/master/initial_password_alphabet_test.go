package master

import (
	"regexp"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/auth"
)

// A generated password must survive a terminal double-click: letters and
// digits only, so the whole word is selected and copied.
func TestInitialPasswordsCopyAsOneWord(t *testing.T) {
	word := regexp.MustCompile(`^[A-Za-z0-9]{24}$`)
	seen := map[string]bool{}
	for range 200 {
		password, hash, err := initialCredential()
		if err != nil {
			t.Fatal(err)
		}
		if !word.MatchString(password) || seen[password] {
			t.Fatalf("generated password %q is not a unique 24-character word", password)
		}
		seen[password] = true
		if !auth.VerifyPassword(password, hash) {
			t.Fatal("hash does not verify")
		}
	}
}
