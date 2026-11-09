package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
)

// revokedGuidance is written to the service log when the Master definitively
// rejects this Agent, so an operator knows why the service stopped. Install.ps1
// has no uninstall option, so Windows names the manual removal instead.
func revokedGuidance(goos, serviceName string) string {
	const stopped = "This Agent was removed from the cluster or its enrollment was revoked on the Master, so the service has stopped and will not restart automatically. " +
		"To rejoin, run a new join command from Nodes > Add node on the Master; "
	if goos != "windows" {
		return stopped + "to remove this Agent, run the installer with --uninstall --role agent"
	}
	if serviceName == "" {
		serviceName = "OSSTransferAgent"
	}
	return stopped + "to remove this Agent, run in an administrator PowerShell: Stop-Service -Name '" + serviceName + "'; sc.exe delete '" + serviceName +
		"'; then delete its installation folder (by default $env:ProgramData\\OSS Transfer\\agent)"
}

const revokedContainerGuidance = "This Agent was removed from the cluster or its enrollment was revoked on the Master. " +
	"The container stays idle until it is stopped: remove it (docker rm -f, or docker compose down), or rejoin with a new join command from Nodes > Add node"

type revokedError struct {
	err      error
	guidance string
}

func (e revokedError) Error() string { return e.err.Error() + ". " + e.guidance }
func (e revokedError) Unwrap() error { return e.err }

// containerMarker exists only in the published image. A container restart
// policy (restart: unless-stopped) restarts any exit, so a revoked Agent in a
// container stays idle until the container is stopped instead.
var containerMarker = "/usr/local/share/oss-transfer/provenance.json"

// revocation stops the service supervisor where it cannot be configured to
// skip restarts by exit status, and annotates the error for the service log.
func revocation(ctx context.Context, err error, serviceName string, log io.Writer) error {
	if !errors.Is(err, agenttransport.ErrRevoked) {
		return err
	}
	if info, statErr := os.Stat(containerMarker); statErr == nil && info.Mode().IsRegular() {
		fmt.Fprintf(log, "%v. %s\n", err, revokedContainerGuidance)
		<-ctx.Done()
		return nil
	}
	stopSupervisor(serviceName)
	return revokedError{err, revokedGuidance(runtime.GOOS, serviceName)}
}

// exitStatus maps a revoked enrollment to a status the service manager does
// not restart: systemd units list it in RestartPreventExitStatus, and launchd
// restarts only unsuccessful exits (see revokedExitStatus per platform).
func exitStatus(err error) int {
	if errors.Is(err, agenttransport.ErrRevoked) {
		return revokedExitStatus
	}
	return 1
}

// signalSupervisor asks a restarting supervisor (FreeBSD daemon -r) to exit.
// It acts only on the supervisor recorded in pidfile that is this process's
// parent, so an unrelated process is never signalled.
func signalSupervisor(pidfile string, parent int, kill func(int) error) bool {
	if parent <= 1 {
		return false
	}
	info, err := os.Lstat(pidfile)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64 {
		return false
	}
	raw, err := os.ReadFile(pidfile)
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid != parent {
		return false
	}
	return kill(pid) == nil
}
