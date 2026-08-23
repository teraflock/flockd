//go:build windows

package svc

import (
	"context"
	"fmt"
)

func newPlatformManager() Manager { return &scmManager{} }

// scmManager is the Windows Service Control Manager integration.
//
// TODO(windows): implement with golang.org/x/sys/windows/svc/mgr —
// CreateService with delayed auto-start, service recovery actions, and an
// event-log hook. Until then every operation returns ErrUnsupported with
// manual instructions.
type scmManager struct{}

func (scmManager) Install(context.Context, string, []string, Options) error {
	return fmt.Errorf("%w: run `flockd --standalone` in a terminal, or create a service manually with `sc.exe create flockd binPath= \"C:\\path\\to\\flockd.exe\"`", ErrUnsupported)
}

func (scmManager) Uninstall(context.Context) error {
	return fmt.Errorf("%w: remove with `sc.exe delete flockd`", ErrUnsupported)
}

func (scmManager) Start(context.Context) error {
	return fmt.Errorf("%w: start with `sc.exe start flockd`", ErrUnsupported)
}

func (scmManager) Stop(context.Context) error {
	return fmt.Errorf("%w: stop with `sc.exe stop flockd`", ErrUnsupported)
}

func (scmManager) Status(context.Context) (Status, error) {
	return StatusUnknown, ErrUnsupported
}
