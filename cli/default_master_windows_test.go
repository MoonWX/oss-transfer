package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestPublicConnectionACLRequiresTrustedOwnerAndWriters(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, owner, rules string
		wantErr            bool
	}{
		{"owner-rights", user.User.Sid.String(), "(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;OW)", false},
		{"system-owner-rights", "SY", "(A;;FA;;;OW)", false},
		{"administrator-owner-rights", "BA", "(A;;FA;;;OW)", false},
		{"public-read", user.User.Sid.String(), "(A;;FA;;;OW)(A;;FR;;;WD)", false},
		{"untrusted-owner", "BU", "(A;;FA;;;OW)", true},
		{"everyone-write", user.User.Sid.String(), "(A;;FA;;;OW)(A;;FW;;;WD)", true},
		{"users-write", user.User.Sid.String(), "(A;;FA;;;OW)(A;;FW;;;BU)", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			sd, err := windows.SecurityDescriptorFromString("O:" + test.owner + "D:P" + test.rules)
			if err != nil {
				t.Fatal(err)
			}
			err = checkPublicConnectionACL(sd, user.User.Sid)
			if test.wantErr && !errors.Is(err, os.ErrPermission) || !test.wantErr && err != nil {
				t.Fatalf("unexpected ACL result: %v", err)
			}
		})
	}
}

func TestDefaultMasterDiscoveryAcceptsOwnerRightsDirectory(t *testing.T) {
	directory, cli := discoveryTestPaths(t)
	// Match Python 3.13's mkdir(mode=0700), including inheritance on files.
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;OW)")
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(directory, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	writeTestConnection(t, cli, "master", "http://127.0.0.1:1688")
	got, err := discoveredMaster()
	if err != nil || got != "http://127.0.0.1:1688" {
		t.Fatalf("owner-only installation discovery: origin=%q error=%v", got, err)
	}
	// Directory ownership alone never permits another principal to replace
	// a public cluster file and redirect a privileged CLI to another server.
	unsafe, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;OW)(A;;FW;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	unsafeACL, _, err := unsafe.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(filepath.Clean(cli+".cluster.json"), windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, unsafeACL, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := discoveredMaster(); err == nil {
		t.Fatal("publicly writable connection file accepted")
	}
}
