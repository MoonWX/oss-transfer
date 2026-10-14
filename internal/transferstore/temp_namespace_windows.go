//go:build windows

package transferstore

import (
	"errors"
	"os"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const tempNamespaceFullAccess = windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x1ff

// Supply the protected DACL at creation. Tightening an inherited ACL after
// mkdir would leave a window in which another account can acquire a writable
// handle and retain it after the permissions have been restricted.
func createTempNamespace(parent *os.Root, name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) || platformKey(name) != nil {
		return ErrTempIdentity
	}
	directory, err := parent.Open(".")
	if err != nil {
		return err
	}
	defer directory.Close()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	sid := user.User.Sid.String()
	sddl := "O:" + sid + "D:P(A;OICI;FA;;;SY)"
	if sid != "S-1-5-18" {
		sddl += "(A;OICI;FA;;;" + sid + ")"
	}
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	unicodeName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return err
	}
	attributes := windows.OBJECT_ATTRIBUTES{RootDirectory: windows.Handle(directory.Fd()), ObjectName: unicodeName, SecurityDescriptor: sd}
	attributes.Length = uint32(unsafe.Sizeof(attributes))
	var handle windows.Handle
	err = windows.NtCreateFile(&handle, windows.SYNCHRONIZE|windows.FILE_READ_ATTRIBUTES, &attributes, &windows.IO_STATUS_BLOCK{}, nil,
		windows.FILE_ATTRIBUTE_DIRECTORY, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_CREATE, windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	runtime.KeepAlive(sd)
	runtime.KeepAlive(user)
	runtime.KeepAlive(directory)
	if err != nil {
		return tempNTError(err)
	}
	return windows.CloseHandle(handle)
}

// Creation already supplied the protected ACL. Reopening a directory with
// additional security rights is unnecessary and can fail on Windows; validate
// the original protected owner/DACL through its existing confined handle.
func secureTempNamespace(root *os.Root) error { return verifyTempNamespace(root) }

func verifyTempNamespace(root *os.Root) error {
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	defer runtime.KeepAlive(sd)
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	current := user.User.Sid.String()
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || owner.String() != current {
		return errors.New("temporary namespace belongs to another Windows account")
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		return errors.New("temporary namespace permissions are inherited")
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil {
		return errors.New("temporary namespace has no private ACL")
	}
	expected := map[string]bool{current: false, "S-1-5-18": false}
	if int(acl.AceCount) != len(expected) {
		return errors.New("temporary namespace has additional access rules")
	}
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &ace); err != nil {
			return err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags != windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE || ace.Mask != tempNamespaceFullAccess {
			return errors.New("temporary namespace access rule changed")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.IsValid() {
			return errors.New("temporary namespace access rule has an invalid identity")
		}
		name := sid.String()
		seen, allowed := expected[name]
		if !allowed || seen {
			return errors.New("temporary namespace is accessible to another account")
		}
		expected[name] = true
	}
	return nil
}
