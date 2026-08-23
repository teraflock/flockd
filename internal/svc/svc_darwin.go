//go:build darwin

package svc

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const launchdLabel = "dev.teraflock.flockd"

func newPlatformManager() Manager { return &launchdManager{} }

// launchdManager manages a per-user LaunchAgent (no root needed; the
// daemon serves loopback only).
type launchdManager struct{}

func (m *launchdManager) plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("svc: home dir: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"), nil
}

// renderPlist is separated for testability.
//
// ProcessType is deliberately Standard, not Background: Background (and
// LowPriorityBackgroundIO, both tried first) throttled the first-run model
// download ~20x and starved token streaming. Being polite to the operator's
// machine is the governor's job (idle detection, instant-yield, battery
// guard) — not launchd's blunt QoS tier.
func renderPlist(binPath string, args []string, opts Options) string {
	var argsXML strings.Builder
	argsXML.WriteString("\t\t<string>" + binPath + "</string>\n")
	for _, a := range args {
		argsXML.WriteString("\t\t<string>" + a + "</string>\n")
	}
	logXML := ""
	if opts.LogPath != "" {
		logXML = fmt.Sprintf("\t<key>StandardOutPath</key>\n\t<string>%s</string>\n"+
			"\t<key>StandardErrorPath</key>\n\t<string>%s</string>\n", opts.LogPath, opts.LogPath)
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
%s	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ProcessType</key>
	<string>Standard</string>
%s</dict>
</plist>
`, launchdLabel, argsXML.String(), logXML)
}

func (m *launchdManager) Install(ctx context.Context, binPath string, args []string, opts Options) error {
	path, err := m.plistPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("svc: mkdir LaunchAgents: %w", err)
	}
	if err := os.WriteFile(path, []byte(renderPlist(binPath, args, opts)), 0o644); err != nil {
		return fmt.Errorf("svc: write plist: %w", err)
	}
	return nil
}

func (m *launchdManager) Uninstall(ctx context.Context) error {
	_ = m.Stop(ctx)
	path, err := m.plistPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("svc: remove plist: %w", err)
	}
	return nil
}

func (m *launchdManager) Start(ctx context.Context) error {
	path, err := m.plistPath()
	if err != nil {
		return err
	}
	if out, err := exec.CommandContext(ctx, "launchctl", "load", "-w", path).CombinedOutput(); err != nil {
		return fmt.Errorf("svc: launchctl load: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (m *launchdManager) Stop(ctx context.Context) error {
	path, err := m.plistPath()
	if err != nil {
		return err
	}
	if out, err := exec.CommandContext(ctx, "launchctl", "unload", path).CombinedOutput(); err != nil {
		return fmt.Errorf("svc: launchctl unload: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (m *launchdManager) Status(ctx context.Context) (Status, error) {
	path, err := m.plistPath()
	if err != nil {
		return StatusUnknown, err
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return StatusNotInstalled, nil
	}
	out, err := exec.CommandContext(ctx, "launchctl", "list").Output()
	if err != nil {
		return StatusUnknown, fmt.Errorf("svc: launchctl list: %w", err)
	}
	if strings.Contains(string(out), launchdLabel) {
		return StatusRunning, nil
	}
	return StatusStopped, nil
}
