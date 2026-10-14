//go:build windows

package transferstore

import (
	"io/fs"
	"os"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows exposes no POSIX directory fsync. The temporary and published file
// handles are flushed by Write; directory durability relies on the filesystem
// journal and recovery must still validate the target object.
func syncTempNamespace(_ *os.Root) error { return nil }

// Both names are single components under pinned directory handles. Reopening
// the source relative to its original directory prevents a renamed staging
// directory from redirecting publication. Its identity is checked again before
// the kernel rename/link operation, which uses that same file handle.
func publishTempAcrossRoots(source, destination *os.Root, name, target string, overwrite bool, expectedIdentity string) error {
	for _, component := range []string{name, target} {
		if component == "" || component == "." || component == ".." || strings.ContainsAny(component, `/\`) || platformKey(component) != nil {
			return ErrInvalidKey
		}
	}
	if expectedIdentity == "" {
		return ErrTempIdentity
	}
	sourceDir, err := source.Open(".")
	if err != nil {
		return err
	}
	defer sourceDir.Close()
	targetDir, err := destination.Open(".")
	if err != nil {
		return err
	}
	defer targetDir.Close()
	unicodeName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return err
	}
	attributes := windows.OBJECT_ATTRIBUTES{RootDirectory: windows.Handle(sourceDir.Fd()), ObjectName: unicodeName}
	attributes.Length = uint32(unsafe.Sizeof(attributes))
	var handle windows.Handle
	access := uint32(windows.SYNCHRONIZE | windows.FILE_READ_ATTRIBUTES | windows.FILE_WRITE_ATTRIBUTES)
	if overwrite {
		access |= windows.DELETE
	}
	err = windows.NtCreateFile(&handle, access, &attributes, &windows.IO_STATUS_BLOCK{}, nil, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	if err != nil {
		return tempNTError(err)
	}
	defer windows.CloseHandle(handle)
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return err
	}
	if information.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 {
		return ErrTempIdentity
	}
	// NewFile owns the supplied handle, so use a duplicate for the existing
	// identity helper; the original remains open for the publication call.
	var duplicate windows.Handle
	process := windows.CurrentProcess()
	if err := windows.DuplicateHandle(process, handle, process, &duplicate, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
		return err
	}
	file := os.NewFile(uintptr(duplicate), name)
	identity, err := handleIdentity(file)
	file.Close()
	if err != nil {
		return err
	}
	if identity != expectedIdentity {
		return ErrTempIdentity
	}
	utf16, err := windows.UTF16FromString(target)
	if err != nil {
		return err
	}
	if len(utf16) > 256 {
		return fs.ErrInvalid
	}
	// FILE_RENAME_INFORMATION and FILE_LINK_INFORMATION share this layout.
	// Flags=0 is ReplaceIfExists=false for class 11 (FileLinkInformation).
	info := struct {
		Flags          uint32
		RootDirectory  windows.Handle
		FileNameLength uint32
		FileName       [256]uint16
	}{RootDirectory: windows.Handle(targetDir.Fd()), FileNameLength: uint32((len(utf16) - 1) * 2)}
	copy(info.FileName[:], utf16)
	class := uint32(11)
	if overwrite {
		class = 65 // FileRenameInformationEx
		info.Flags = windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS
	}
	err = windows.NtSetInformationFile(handle, &windows.IO_STATUS_BLOCK{}, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), class)
	if overwrite && (err == windows.STATUS_INVALID_INFO_CLASS || err == windows.STATUS_INVALID_PARAMETER || err == windows.STATUS_NOT_SUPPORTED) {
		info.Flags = 1 // ReplaceIfExists=true, FileRenameInformation.
		err = windows.NtSetInformationFile(handle, &windows.IO_STATUS_BLOCK{}, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), 10)
	}
	runtime.KeepAlive(sourceDir)
	runtime.KeepAlive(targetDir)
	return tempNTError(err)
}

func tempNTError(err error) error {
	if status, ok := err.(windows.NTStatus); ok {
		return status.Errno()
	}
	return err
}
