//go:build windows

package softwareupdate

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

func hostSupport() error {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return &capabilityError{"PERMISSION", "The native service needs administrator privileges to update itself"}
	}
	if _, err := exec.LookPath("powershell.exe"); err != nil {
		return &capabilityError{"MISSING_DEPENDENCY", "Windows PowerShell is required by the native installer"}
	}
	return nil
}
func checkInstallationPrivileges(installation) error { return nil }
func protectDirectory(path string) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString(fmt.Sprintf("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;%s)", user.User.Sid.String()))
	if err != nil {
		return err
	}
	acl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
}
func launchHelper(helper, request, logPath string) error {
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.Command(helper, "__apply-update", request)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP}
	cmd.Stdout, cmd.Stderr = log, log
	if err = cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
func terminateInstaller(cmd *exec.Cmd) error { return cmd.Process.Signal(os.Interrupt) }
