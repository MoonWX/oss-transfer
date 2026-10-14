//go:build windows

package transferstore

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestTempNamespaceWindowsPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private")
	parent, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if err := createTempNamespace(parent, "private"); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := verifyTempNamespace(root); err != nil {
		t.Fatalf("namespace not private at creation: %v", err)
	}
	if err := secureTempNamespace(root); err != nil {
		t.Fatal(err)
	}
	if err := verifyTempNamespace(root); err != nil {
		t.Fatal(err)
	}
	file, err := root.OpenFile("payload", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	// An extra Everyone rule must prevent recovery cleanup, even though the
	// current process still has access and the directory identity is unchanged.
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;WD)")
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
	if verifyTempNamespace(root) == nil {
		t.Fatal("namespace with external write access was accepted")
	}
}
