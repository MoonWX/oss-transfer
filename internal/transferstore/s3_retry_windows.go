//go:build windows

package transferstore

import "syscall"

var platformTransientErrnos = []error{syscall.WSAECONNRESET, syscall.WSAECONNABORTED}
