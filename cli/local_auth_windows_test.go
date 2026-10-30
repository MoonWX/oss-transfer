package main

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestLocalAuthWindowsAllowsServiceAdministratorACLOnly(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, rules string
		wantErr     bool
	}{
		{"service", "(A;;FA;;;SY)(A;;FA;;;BA)", false},
		{"public-read", "(A;;FA;;;SY)(A;;FA;;;BA)(A;;FR;;;BU)", true},
		{"public-write", "(A;;FA;;;SY)(A;;FA;;;BA)(A;;FW;;;WD)", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "local-auth.json")
			if err := os.WriteFile(path, []byte("credential"), 0600); err != nil {
				t.Fatal(err)
			}
			sd, err := windows.SecurityDescriptorFromString("D:P" + test.rules + "(A;;FA;;;" + user.User.Sid.String() + ")")
			if err != nil {
				t.Fatal(err)
			}
			acl, _, err := sd.DACL()
			if err != nil {
				t.Fatal(err)
			}
			if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := privateLocalAuthFile(path, info); (err != nil) != test.wantErr {
				t.Fatalf("unexpected permissions result: %v", err)
			}
		})
	}
}
