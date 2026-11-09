package main

import (
	"os"
	"syscall"
)

// daemon(8) restarts its child regardless of the exit status. The installer's
// rc.d script records the supervisor at /var/run/NAME.supervisor.pid.
const revokedExitStatus = 78

func stopSupervisor(serviceName string) {
	signalSupervisor("/var/run/"+serviceName+".supervisor.pid", os.Getppid(), func(pid int) error {
		return syscall.Kill(pid, syscall.SIGTERM)
	})
}
