//go:build windows

package servicehost

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Model the installer's inheritable ACL, then secure that same directory from
// the service. LocalSystem must retain read access to both the existing startup
// request and files subsequently created by the installing administrator.
func TestSecuringDirectoryRetainsSystemAccessToInheritedFiles(t *testing.T) {
	directory := t.TempDir()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString(fmt.Sprintf("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;%s)", user.User.Sid.String()))
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(directory, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	request := filepath.Join(directory, "ready.request")
	if err := os.WriteFile(request, []byte("startup nonce"), 0600); err != nil {
		t.Fatal(err)
	}
	assertSystemRead(t, request)
	if err := privateDirectory(directory); err != nil {
		t.Fatal(err)
	}
	assertSystemRead(t, request)
	next := filepath.Join(directory, "next.request")
	if err := os.WriteFile(next, []byte("next nonce"), 0600); err != nil {
		t.Fatal(err)
	}
	assertSystemRead(t, next)
}

func assertSystemRead(t *testing.T, path string) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil {
		t.Fatalf("missing file ACL: %v", err)
	}
	for i := uint16(0); i < acl.AceCount; i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, uint32(i), &ace); err != nil {
			t.Fatal(err)
		}
		if ace.Header.AceType == windows.ACCESS_ALLOWED_ACE_TYPE && ace.Mask&windows.FILE_READ_DATA != 0 && (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String() == "S-1-5-18" {
			return
		}
	}
	t.Fatalf("LocalSystem cannot read %s: %s", filepath.Base(path), sd.String())
}
