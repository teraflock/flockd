//go:build linux

package governor

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// NewPlatformSleepInhibitor returns the Linux inhibitor: a logind
// `--what=idle` inhibitor lock, taken through `systemd-inhibit` the way
// the idle source reads logind through `loginctl` (no D-Bus client
// library). `idle` is the one logind inhibitor class that matches the
// policy: it stops the *automatic* idle action (suspend after N minutes
// of no input) and nothing else — `sleep` would also block or delay a
// suspend the operator asks for, which the policy forbids. The lock is
// held while the child runs; the child is `tail --pid=<flockd pid>`, so
// it ends on its own if flockd dies without releasing. `systemd-inhibit
// --list` shows it as flockd with the reason. Desktops that consult only
// their own session inhibitors (not logind's) may sleep anyway; see
// docs/sleep.md.
func NewPlatformSleepInhibitor(log *slog.Logger) SleepInhibitor {
	return &execInhibitor{log: log, spawn: systemdInhibit}
}

func systemdInhibit(ctx context.Context, reason string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "systemd-inhibit",
		"--what=idle", "--who=flockd", "--why="+reason, "--mode=block",
		"tail", "--pid="+strconv.Itoa(os.Getpid()), "-f", "/dev/null")
	// Release kills the whole group: systemd-inhibit drops the lock when
	// it exits, but its tail child would otherwise linger until flockd
	// does.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	return cmd
}
