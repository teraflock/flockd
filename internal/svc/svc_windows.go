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

func (scmManager) Install(context.Context, string, []string) error {
	return fmt.Errorf("%w: run `hived --standalone` in a terminal, or create a service manually with `sc.exe create hived binPath= \"C:\\path\\to\\hived.exe\"`", ErrUnsupported)
}

func (scmManager) Uninstall(context.Context) error {
	return fmt.Errorf("%w: remove with `sc.exe delete hived`", ErrUnsupported)
}

func (scmManager) Start(context.Context) error {
	return fmt.Errorf("%w: start with `sc.exe start hived`", ErrUnsupported)
}

func (scmManager) Stop(context.Context) error {
	return fmt.Errorf("%w: stop with `sc.exe stop hived`", ErrUnsupported)
}

func (scmManager) Status(context.Context) (Status, error) {
	return StatusUnknown, ErrUnsupported
}
