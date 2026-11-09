package main

// launchd restarts an Agent only after an unsuccessful exit (KeepAlive
// SuccessfulExit=false), so a revoked Agent reports success.
const revokedExitStatus = 0

func stopSupervisor(string) {}
