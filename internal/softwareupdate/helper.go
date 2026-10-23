package softwareupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

func RunHelper(requestPath string) error {
	return runHelper(requestPath, newClient(), runInstaller, checkHost)
}

func runHelper(requestPath string, client *client, execute func(*exec.Cmd) error, support func() error) (runErr error) {
	var req request
	if err := readJSON(requestPath, &req); err != nil {
		return err
	}
	work := filepath.Join(req.Options.DataDirectory, "runtime", "updates", req.ID)
	if !idPattern.MatchString(req.ID) || !stableVersion.MatchString(req.Version) || compareVersions(req.Version, req.Options.Version) <= 0 || requestPath != filepath.Join(work, "request.json") {
		return errors.New("Invalid update request identity")
	}
	statusPath := filepath.Join(filepath.Dir(work), "status.json")
	setStatus := func(phase string, err error) error {
		var current Status
		if readErr := readJSON(statusPath, &current); readErr != nil {
			return readErr
		}
		if current.ID != req.ID {
			return errors.New("A newer update request has replaced this request")
		}
		current.Phase, current.UpdatedAt = phase, time.Now().UTC()
		if err != nil {
			current.Error = err.Error()
			current.Log = logTail(filepath.Join(work, "installer.log"))
		}
		return writeJSON(statusPath, current)
	}
	defer func() {
		if runErr != nil {
			_ = setStatus("FAILED", runErr)
		}
	}()
	installed, err := inspectInstallation(req.Options, req.Executable)
	if err != nil {
		return err
	}
	if err = support(); err != nil {
		return err
	}
	if err = checkInstallationPrivileges(installed); err != nil {
		return err
	}
	if err = privateDirectory(work); err != nil {
		return err
	}
	lockPath := installed.CLIPath + ".update.lock"
	if err = regular(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	lock, err := bolt.Open(lockPath, 0600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return errors.New("Another service update is using this installation's CLI; retry after it finishes")
	}
	defer lock.Close()
	if err = setStatus("DOWNLOADING", nil); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	stage, _, err := client.prepare(ctx, work, req.Version, platform())
	if err != nil {
		return err
	}
	// Verify the installation once more after downloading, before the native
	// script touches any service or database.
	if _, err = inspectInstallation(req.Options, req.Executable); err != nil {
		return err
	}
	if err = setStatus("APPLYING", nil); err != nil {
		return err
	}
	log, err := os.OpenFile(filepath.Join(work, "installer.log"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	cmd := installerCommand(req, installed, stage)
	cmd.Stdin = nil
	cmd.Stdout, cmd.Stderr = log, log
	cmd.Dir = work
	err = execute(cmd)
	err = errors.Join(err, log.Close())
	if err != nil {
		return fmt.Errorf("Native upgrade failed; inspect the installer log for its rollback result: %w", err)
	}
	// The installer already verified PID-owned readiness. Confirm that its
	// metadata was actually advanced, rather than treating exit 0 as success.
	nextOptions := req.Options
	nextOptions.Version = req.Version
	next, err := inspectInstallation(nextOptions, req.Executable)
	if err != nil {
		return err
	}
	if next.Version != req.Version {
		return errors.New("Installer exited without installing the requested release")
	}
	if err = setStatus("COMPLETED", nil); err != nil {
		return err
	}
	// Keep small status/log records; release archives and the helper copy can
	// consume hundreds of MiB and are no longer required after verification.
	_ = os.RemoveAll(stage)
	_ = os.Remove(filepath.Join(work, "update-helper"+executableSuffix()))
	return nil
}

func installerCommand(req request, installed installation, stage string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", filepath.Join(stage, "Install.ps1"), "-Upgrade", "-Role", req.Options.Role, "-InstallDir", filepath.Dir(req.Executable), "-PackageDir", stage, "-Yes")
	}
	args := []string{filepath.Join(stage, "native-install.sh"), "--upgrade", "--role", req.Options.Role, "--install-dir", filepath.Dir(req.Executable), "--package-dir", stage, "--yes"}
	if runtime.GOOS == "darwin" && strings.Contains(filepath.ToSlash(installed.Unit), "/Library/LaunchAgents/") {
		args = append(args, "--user")
	}
	return exec.Command("bash", args...)
}

func runInstaller(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(15 * time.Minute):
		// Give the installer's rollback handler time to restore files and its
		// original service. Killing the shell immediately would skip rollback.
		_ = terminateInstaller(cmd)
		select {
		case err := <-done:
			return errors.Join(errors.New("Native upgrade exceeded its time limit"), err)
		case <-time.After(5 * time.Minute):
			_ = cmd.Process.Kill()
			<-done
			return errors.New("Native upgrade and rollback timed out; inspect the service and retained backup")
		}
	}
}

func logTail(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return ""
	}
	if info.Size() > 8192 {
		_, _ = file.Seek(-8192, io.SeekEnd)
	}
	raw, _ := io.ReadAll(io.LimitReader(file, 8192))
	return string(raw)
}
