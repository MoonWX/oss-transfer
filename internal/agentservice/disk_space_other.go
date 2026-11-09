//go:build !linux && !darwin && !freebsd && !windows

package agentservice

func diskSpace(string) (free, total int64, ok bool) { return 0, 0, false }
