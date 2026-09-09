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

const unitName = "flockd.service"

// warn/info are the nil-safe Options.Warn/Info callers. They live here
// because the systemd lingering flow is their only caller; on the other
// platforms they would be dead code.
func (o Options) warn(msg string) {
	if o.Warn != nil {
		o.Warn(msg)
	}
}

func (o Options) info(msg string) {
	if o.Info != nil {
		o.Info(msg)
	}
}

func newPlatformManager() Manager { return &systemdManager{} }

// systemdManager manages a user-level systemd unit
// (~/.config/systemd/user/flockd.service).
type systemdManager struct{}

func (*systemdManager) unitPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("svc: home dir: %w", err)
	}
	return filepath.Join(home, ".config", "systemd", "user", unitName), nil
}

func renderUnit(binPath string, args []string) string {
	return fmt.Sprintf(`[Unit]
Description=Teraflock node daemon
After=network-online.target
Wants=network-online.target
# Cap the restart storm on permanent failures (missing runtime for
# this OS/arch, bad config): after 5 failed starts in 60s systemd
# marks the unit failed and stops retrying, so tera status can see
# it instead of the daemon hammering the CPU forever.
StartLimitBurst=5
StartLimitIntervalSec=60

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

func (m *systemdManager) Install(ctx context.Context, binPath string, args []string, opts Options) error {
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
	ensureLinger(ctx, opts)
	return nil
}

// ensureLinger keeps a --user unit alive after the operator's last session
// ends: without lingering, systemd tears the user manager down at logout,
// so a node started over SSH dies when the session closes — exactly the
// headless case where the governor's assume-idle default is right. Failure
// is a warning, never an error: a desktop user who prefers non-lingering
// should not be blocked, and a polkit prompt over SSH must not hang
// `tera up`. `tera down` deliberately never disables lingering (other
// units may rely on it).
func ensureLinger(ctx context.Context, opts Options) {
	user := os.Getenv("USER")
	if user == "" {
		if u, err := os.UserHomeDir(); err == nil {
			user = filepath.Base(u)
		}
	}
	hint := "flockd will stop when you log out; run `sudo loginctl enable-linger " + user + "` to keep it running"
	out, err := exec.CommandContext(ctx, "loginctl", "show-user", user, "-p", "Linger").Output()
	if err == nil {
		if on, ok := parseLinger(string(out)); ok && on {
			opts.info("lingering already enabled: flockd keeps running after you log out")
			return
		}
	}
	// No username: acts on the calling user, which needs no root on
	// systemd >= 230 unless polkit says otherwise.
	if out, err := exec.CommandContext(ctx, "loginctl", "enable-linger").CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		opts.warn(hint + " (" + msg + ")")
		return
	}
	opts.info("lingering enabled: flockd keeps running after you log out")
}

// parseLinger reads `loginctl show-user <u> -p Linger` output
// ("Linger=yes\n"). ok is false when the property is absent, so callers
// fall through to enable-linger rather than trusting a blank answer.
func parseLinger(out string) (on, ok bool) {
	for _, line := range strings.Split(out, "\n") {
		k, v, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found || k != "Linger" {
			continue
		}
		return strings.EqualFold(strings.TrimSpace(v), "yes"), true
	}
	return false, false
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

func (*systemdManager) Start(ctx context.Context) error {
	if out, err := exec.CommandContext(ctx, "systemctl", "--user", "enable", "--now", unitName).CombinedOutput(); err != nil {
		return fmt.Errorf("svc: systemctl enable --now: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (*systemdManager) Stop(ctx context.Context) error {
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
