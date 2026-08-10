//go:build linux

package svc

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const unitName = "hived.service"

func newPlatformManager() Manager { return &systemdManager{} }

// systemdManager manages a user-level systemd unit
// (~/.config/systemd/user/hived.service).
type systemdManager struct{}

func (m *systemdManager) unitPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("svc: home dir: %w", err)
	}
	return filepath.Join(home, ".config", "systemd", "user", unitName), nil
}

func renderUnit(binPath string, args []string) string {
	return fmt.Sprintf(`[Unit]
Description=HiveGrid node daemon
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%s %s
Restart=on-failure
RestartSec=5
# Never degrade the operator's machine (SPEC §10).
Nice=10
IOSchedulingClass=idle

[Install]
WantedBy=default.target
`, binPath, strings.Join(args, " "))
}

func (m *systemdManager) Install(ctx context.Context, binPath string, args []string) error {
	path, err := m.unitPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("svc: mkdir systemd user dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(renderUnit(binPath, args)), 0o644); err != nil {
		return fmt.Errorf("svc: write unit: %w", err)
	}
	if out, err := exec.CommandContext(ctx, "systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("svc: daemon-reload: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (m *systemdManager) Uninstall(ctx context.Context) error {
	_ = m.Stop(ctx)
	path, err := m.unitPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("svc: remove unit: %w", err)
	}
	_ = exec.CommandContext(ctx, "systemctl", "--user", "daemon-reload").Run()
	return nil
}

func (m *systemdManager) Start(ctx context.Context) error {
	if out, err := exec.CommandContext(ctx, "systemctl", "--user", "enable", "--now", unitName).CombinedOutput(); err != nil {
		return fmt.Errorf("svc: systemctl enable --now: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (m *systemdManager) Stop(ctx context.Context) error {
	if out, err := exec.CommandContext(ctx, "systemctl", "--user", "disable", "--now", unitName).CombinedOutput(); err != nil {
		return fmt.Errorf("svc: systemctl disable --now: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (m *systemdManager) Status(ctx context.Context) (Status, error) {
	path, err := m.unitPath()
	if err != nil {
		return StatusUnknown, err
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return StatusNotInstalled, nil
	}
	out, _ := exec.CommandContext(ctx, "systemctl", "--user", "is-active", unitName).Output()
	if strings.TrimSpace(string(out)) == "active" {
		return StatusRunning, nil
	}
	return StatusStopped, nil
}
