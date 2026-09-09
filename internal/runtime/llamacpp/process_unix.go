//go:build !windows

package llamacpp

import (
	"os/exec"
	"syscall"
)

// childGuard is the Windows job object that ties llama-server's lifetime
// to the daemon's (process_windows.go). Unix needs nothing: stop() sends
// SIGTERM then SIGKILL, and a crashed daemon leaves the child reparented
// to init where the next supervisor's port probe never collides with it.
type childGuard struct{}

func newChildGuard() (*childGuard, error) { return &childGuard{}, nil }

func (*childGuard) adopt(*exec.Cmd) error { return nil }

func (*childGuard) Close() error { return nil }

func childSysProcAttr() *syscall.SysProcAttr { return nil }
