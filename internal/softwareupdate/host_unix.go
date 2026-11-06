//go:build !windows

package softwareupdate

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
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

// The helper sets its own umask: systemd before 240 is not given UMask for the
// transient unit, and launchd and rc.d helpers inherit the service's umask.
func privateHelperProcess() { syscall.Umask(0077) }

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
		cmd = exec.Command("systemd-run", systemdRunArguments(systemdVersion(), "oss-transfer-update-"+filepath.Base(filepath.Dir(request)), helper, request)...)
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

var systemdVersionLine = regexp.MustCompile(`^systemd ([0-9]+)\b`)

// systemdVersion returns 0 when unknown, which selects only the options that
// systemd 219 (CentOS 7 / RHEL 7) accepts.
func systemdVersion() int {
	output, err := exec.Command("systemd-run", "--version").Output()
	if err != nil {
		return 0
	}
	return parseSystemdVersion(string(output))
}

func parseSystemdVersion(output string) int {
	first, _, _ := strings.Cut(output, "\n")
	match := systemdVersionLine.FindStringSubmatch(strings.TrimSpace(first))
	if match == nil {
		return 0
	}
	version, err := strconv.Atoi(match[1])
	if err != nil {
		return 0
	}
	return version
}

// --collect (systemd 236) unloads a failed helper unit; every update uses a
// new unit name, so older systems merely keep a failed unit listed. Type=exec
// (240) reports a failed exec; older systems start the helper as Type=simple.
func systemdRunArguments(version int, unit, helper, request string) []string {
	args := []string{"--quiet"}
	if version >= 236 {
		args = append(args, "--collect")
	}
	args = append(args, "--unit="+unit)
	if version >= 240 {
		args = append(args, "--property=Type=exec", "--property=UMask=0077")
	}
	return append(args, helper, "__apply-update", request)
}
func terminateInstaller(cmd *exec.Cmd) error { return cmd.Process.Signal(syscall.SIGTERM) }
