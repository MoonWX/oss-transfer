//go:build !windows

package agentservice

import (
	"syscall"
	"time"
)

// processCPU returns the user plus system CPU time used by this process.
func processCPU() time.Duration {
	var usage syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &usage) != nil {
		return 0
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}
