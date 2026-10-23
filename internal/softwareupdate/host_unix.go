//go:build !windows

package softwareupdate

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

func hostSupport() error {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" && runtime.GOOS != "freebsd" {
		return &capabilityError{"UNSUPPORTED", "Online updates are unavailable on this platform"}
	}
	if runtime.GOOS != "darwin" && os.Geteuid() != 0 {
		return &capabilityError{"PERMISSION", "The native service needs root privileges to update itself"}
	}
	if runtime.GOOS == "linux" {
		if _, err := os.Stat("/run/systemd/system"); err != nil {
			return &capabilityError{"UNMANAGED", "Online updates require a systemd-managed service"}
		}
		if _, err := exec.LookPath("systemd-run"); err != nil {
			return &capabilityError{"MISSING_DEPENDENCY", "systemd-run is required to restart the service independently"}
		}
	}
	if _, err := exec.LookPath("bash"); err != nil {
		return &capabilityError{"MISSING_DEPENDENCY", "bash is required by the native installer"}
	}
	return nil
}
func checkInstallationPrivileges(installed installation) error {
	if runtime.GOOS == "darwin" && os.Geteuid() != 0 && !strings.Contains(installed.Unit, "/Library/LaunchAgents/") {
		return &capabilityError{"PERMISSION", "The system service needs root privileges to update itself"}
	}
	return nil
}
func protectDirectory(path string) error { return os.Chmod(path, 0700) }
func launchHelper(helper, request, logPath string) error {
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	var cmd *exec.Cmd
	if runtime.GOOS == "linux" {
		// A transient service lives outside the Master's service cgroup. Merely
		// calling setsid would still be killed by systemctl stop on the Master.
		cmd = exec.Command("systemd-run", "--quiet", "--collect", "--unit=oss-transfer-update-"+filepath.Base(filepath.Dir(request)), "--property=Type=exec", "--property=UMask=0077", helper, "__apply-update", request)
		cmd.Stdout, cmd.Stderr = log, log
		return cmd.Run()
	}
	cmd = exec.Command(helper, "__apply-update", request)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout, cmd.Stderr = log, log
	if err = cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
func terminateInstaller(cmd *exec.Cmd) error { return cmd.Process.Signal(syscall.SIGTERM) }
