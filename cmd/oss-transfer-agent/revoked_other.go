//go:build !darwin && !freebsd

package main

// systemd units written by the installer set RestartPreventExitStatus=78.
// Windows services have no recovery actions, so any exit stops the service.
const revokedExitStatus = 78

func stopSupervisor(string) {}
