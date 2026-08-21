// Package svc installs and controls flockd as an OS service: launchd
// (macOS), systemd (Linux) and Windows SCM (stub). `flock up|down|uninstall`
// drive this.
package svc

import (
	"context"
	"errors"
)

// Status of the managed service.
type Status string

const (
	StatusRunning      Status = "running"
	StatusStopped      Status = "stopped"
	StatusNotInstalled Status = "not-installed"
	StatusUnknown      Status = "unknown"
)

// Manager is the platform-specific service controller.
type Manager interface {
	// Install writes the service definition pointing at binPath with args.
	Install(ctx context.Context, binPath string, args []string) error
	Uninstall(ctx context.Context) error
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Status(ctx context.Context) (Status, error)
}

// ErrUnsupported marks platforms whose integration is still a stub.
var ErrUnsupported = errors.New("svc: service management not implemented on this platform")

// NewManager returns the manager for the current OS (svc_*.go).
func NewManager() Manager { return newPlatformManager() }
