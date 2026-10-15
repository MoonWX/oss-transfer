package main

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func testSessionPermissions(t *testing.T, path, host string) {
	t.Helper()
	if err := checkSessionPermissions(path); err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	// Inject Everyone read permission into the file, then its parent. Neither
	// a private file in a writable directory nor a public file is acceptable.
	for _, target := range []string{path, filepath.Dir(path)} {
		sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + user.User.Sid.String() + ")(A;;GR;;;WD)")
		if err != nil {
			t.Fatal(err)
		}
		acl, _, err := sd.DACL()
		if err != nil {
			t.Fatal(err)
		}
		if err := windows.SetNamedSecurityInfo(target, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := loadSession(host); err == nil {
			t.Fatal("public Windows session ACL accepted")
		}
		if err := protectSessionPath(target, target != path); err != nil {
			t.Fatal(err)
		}
		if _, err := loadSession(host); err != nil {
			t.Fatalf("restored private ACL rejected: %v", err)
		}
	}
}
