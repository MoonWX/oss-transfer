//go:build windows

package servicehost

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestLocalCLICredentialHasProtectedAdministrativeACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cli-local-auth.json")
	if err := writeCLILocalAuth(path, "master", "http://127.0.0.1:1685", "private-grant"); err != nil {
		t.Fatal(err)
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("local credential inherited directory access", err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil || acl.AceCount == 0 {
		t.Fatal("missing private credential ACL", err)
	}
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &ace); err != nil {
			t.Fatal(err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			t.Fatal("unexpected private ACL entry")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.Equals(user.User.Sid) && sid.String() != "S-1-5-18" && sid.String() != "S-1-5-32-544" {
			t.Fatal("credential accessible to an ordinary user")
		}
	}
	assertSystemRead(t, path)
	if _, err := os.ReadFile(path); err != nil {
		t.Fatal("service owner cannot read its grant", err)
	}
}
