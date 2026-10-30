package state

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func checkPrivateCredentialKey(path string, _ os.FileInfo) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	return checkCredentialKeyForReader(path, user.User.Sid)
}

func checkCredentialKeyForReader(path string, reader *windows.SID) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	trusted := func(sid *windows.SID) bool {
		return sid != nil && (sid.Equals(reader) || sid.String() == "S-1-5-18" || sid.String() == "S-1-5-32-544")
	}
	owner, _, err := sd.Owner()
	if err != nil || !trusted(owner) {
		return os.ErrPermission
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		return os.ErrPermission
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil || acl.AceCount == 0 {
		return os.ErrPermission
	}
	for index := uint32(0); index < uint32(acl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, index, &ace); err != nil {
			return err
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			continue
		case windows.ACCESS_ALLOWED_ACE_TYPE:
			if !trusted((*windows.SID)(unsafe.Pointer(&ace.SidStart))) && ace.Mask != 0 {
				return os.ErrPermission
			}
		default:
			return os.ErrPermission
		}
	}
	return nil
}

func protectCredentialKey(file *os.File) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	owner := user.User.Sid
	dacl := fmt.Sprintf("D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;%s)", user.User.Sid.String())
	if windows.GetCurrentProcessToken().IsElevated() {
		// An installer runs as an administrator but the service runs as LocalSystem.
		// A stable administrative owner/ACL lets both validate the same private key.
		owner, err = windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
		if err != nil {
			return err
		}
		dacl = "D:P(A;;FA;;;SY)(A;;FA;;;BA)"
	}
	sd, err := windows.SecurityDescriptorFromString(dacl)
	if err != nil {
		return err
	}
	acl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(file.Name(), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, owner, nil, acl, nil)
}
