package main

import (
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

func trustedDiscoveryFile(path string, info os.FileInfo) error {
	for current := path; ; current = filepath.Dir(current) {
		if current != path {
			var err error
			info, err = os.Lstat(current)
			if err != nil {
				return err
			}
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return cliErrorf("集群连接文件权限不安全: %s", path)
		}
		if current == path || current == filepath.Dir(path) {
			if err := publicConnectionACL(current); err != nil {
				return cliErrorf("集群连接文件权限不安全: %s", path)
			}
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	return nil
}

func publicConnectionACL(path string) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	trusted := func(sid *windows.SID) bool {
		return sid != nil && (sid.Equals(user.User.Sid) || sid.String() == "S-1-5-18" || sid.String() == "S-1-5-32-544")
	}
	owner, _, err := sd.Owner()
	if err != nil || !trusted(owner) {
		return os.ErrPermission
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil {
		return os.ErrPermission
	}
	// Public read access is expected. Write/delete/ACL changes belong only to
	// the installing user, Administrators or SYSTEM.
	const writeMask uint32 = 0x10000000 | 0x40000000 | 0x00010000 | 0x00040000 | 0x00080000 | 0x00000100 | 0x00000040 | 0x00000010 | 0x00000004 | 0x00000002
	for index := uint32(0); index < uint32(acl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, index, &ace); err != nil {
			return err
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue
		}
		if ace.Header.AceType == windows.ACCESS_ALLOWED_ACE_TYPE && uint32(ace.Mask)&writeMask != 0 && !trusted((*windows.SID)(unsafe.Pointer(&ace.SidStart))) {
			return os.ErrPermission
		}
	}
	return nil
}
