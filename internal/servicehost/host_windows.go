//go:build windows

package servicehost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

func Run(options Options, program Program) (runErr error) {
	r, err := prepare(options)
	if err != nil {
		return err
	}
	defer func() { r.finish(runErr) }()
	service, err := svc.IsWindowsService()
	if err != nil {
		return err
	}
	if !service {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
		defer cancel()
		return program(ctx, r.ready)
	}
	h := &handler{runtime: r, program: program}
	if err = svc.Run(options.Name, h); err != nil {
		return err
	}
	return h.err
}

type handler struct {
	runtime *runtime
	program Program
	err     error
}

func (h *handler) Execute(_ []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- h.program(ctx, func() error {
			if err := h.runtime.ready(); err != nil {
				return err
			}
			select {
			case ready <- struct{}{}:
			default:
			}
			return nil
		})
	}()
	current := svc.Status{State: svc.StartPending, WaitHint: 30000, CheckPoint: 1}
	status <- current
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	stopping := false
	deadline := time.Now().Add(120 * time.Second)
	for {
		select {
		case <-ready:
			if !stopping {
				current = svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
				status <- current
			}
		case err := <-done:
			if errors.Is(err, context.Canceled) {
				err = nil
			}
			h.err = err
			if err != nil {
				return true, 1
			}
			return false, 0
		case request, ok := <-requests:
			if !ok {
				cancel()
				h.err = errors.New("service control channel closed")
				return true, 1
			}
			switch request.Cmd {
			case svc.Interrogate:
				status <- current
			case svc.Stop, svc.Shutdown:
				if !stopping {
					deadline = time.Now().Add(45 * time.Second)
				}
				stopping = true
				current = svc.Status{State: svc.StopPending, WaitHint: 45000, CheckPoint: 1}
				status <- current
				cancel()
			}
		case <-ticker.C:
			if current.State == svc.StartPending || current.State == svc.StopPending {
				if time.Now().After(deadline) {
					cancel()
					h.err = errors.New("service startup or shutdown timed out")
					return true, 1
				}
				current.CheckPoint++
				status <- current
			}
		}
	}
}
func ownerACL(directory bool) (*windows.ACL, error) {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	inherit := ""
	if directory {
		// Preserve access to inherited children when updating a runtime directory.
		// Without OI/CI, Windows removes inherited ACEs from ready.request before
		// the LocalSystem service can read the installer's startup nonce.
		inherit = "OICI"
	}
	sd, err := windows.SecurityDescriptorFromString(fmt.Sprintf("D:P(A;%s;FA;;;SY)(A;%s;FA;;;BA)(A;%s;FA;;;%s)", inherit, inherit, inherit, user.User.Sid.String()))
	if err != nil {
		return nil, err
	}
	acl, _, err := sd.DACL()
	return acl, err
}
func privateFile(file *os.File) error {
	acl, err := ownerACL(false)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(file.Name(), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
}
func privateDirectory(path string) error {
	acl, err := ownerACL(true)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
}
