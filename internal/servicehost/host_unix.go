//go:build !windows

package servicehost

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

func Run(options Options, program Program) (runErr error) {
	r, err := prepare(options)
	if err != nil {
		return err
	}
	defer func() { r.finish(runErr) }()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return program(ctx, r.ready)
}
func privateFile(file *os.File) error    { return file.Chmod(0600) }
func privateDirectory(path string) error { return os.Chmod(path, 0700) }
