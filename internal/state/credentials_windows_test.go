package state

import (
	"bytes"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestCredentialKeyCreatedByInstallerIsUsableByLocalSystem(t *testing.T) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("administrative installer token required")
	}
	path := filepath.Join(t.TempDir(), "master.db.key")
	if err := writeCredentialKey(path, bytes.Repeat([]byte{7}, 32)); err != nil {
		t.Fatal(err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkCredentialKeyForReader(path, system); err != nil {
		t.Fatal("LocalSystem would reject the installer's key", err)
	}
	// The service and future administrators must not depend on the original
	// administrator's individual SID to accept this database's key.
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := sd.Owner()
	if err != nil || owner.String() != "S-1-5-32-544" {
		t.Fatal("key owner is not the administrative group", err)
	}
	if _, err := readCredentialKey(path); err != nil {
		t.Fatal("creating administrator cannot reopen key", err)
	}
}
