//go:build windows

package svc

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"unicode/utf16"
)

// Windows service management (flockd#28) is a per-user Task Scheduler
// logon task, not an SCM service — decision recorded here and in
// docs/HANDOFF.md.
//
// Why a logon task (plan 03 "Option A"): it is the same shape as the
// macOS LaunchAgent and the systemd --user unit — installed by the
// operator, no UAC prompt, runs in the operator's interactive session.
// That last point is load-bearing: the governor's Windows idle source is
// GetLastInputInfo (sources_windows.go), which is per session. A classic
// SCM service lives in session 0, sees no input ever, and would count the
// machine as idle while the operator is typing — the exact failure the
// "never slows your machine" promise forbids. An SCM mode for headless
// servers (run at boot without logon) needs a session-aware idle source
// first; it is a later, separate issue.
//
// Mechanics: the task definition is rendered as Task Scheduler XML
// (renderTaskXML, tested) and registered with schtasks /Create /XML —
// no COM binding, no cgo. Start/Stop enable+run / end+disable the task,
// so `tera down` also stops it from coming back at the next logon, like
// `systemctl --user disable --now`. Status asks PowerShell for the
// task's State enum, whose name is locale-invariant (schtasks /Query
// prints localized strings). The daemon's output goes to Options.LogPath
// through flockd's --log-file flag: a task has no console, so there is
// nothing to redirect otherwise, and `tera up` tails that same file when
// the daemon fails to come up.
//
// Caveat: schtasks /End is a TerminateProcess of the task's process tree,
// not a graceful shutdown; the daemon keeps no state that needs one, and
// the llama-server child dies with the daemon via its job object
// (internal/runtime/llamacpp/process_windows.go).

// taskName is the Task Scheduler task; plan 07 keeps the daemon's name.
const taskName = "flockd"

func newPlatformManager() Manager { return &taskManager{} }

// taskManager manages the per-user logon task.
type taskManager struct{}

// definitionPath is where the rendered XML is kept for inspection
// (%AppData%\teraflock\flockd.task.xml); the registered task does not
// depend on it.
func (*taskManager) definitionPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("svc: config dir: %w", err)
	}
	return filepath.Join(dir, "teraflock", taskName+".task.xml"), nil
}

// taskUser is the account the task runs as, in DOMAIN\user form.
func taskUser() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("svc: current user: %w", err)
	}
	return u.Username, nil
}

// renderTaskXML builds the Task Scheduler definition. Separated (and
// pure) for the tests, which lock in the restart cap, the log wiring
// and the quoting.
//
//   - LogonTrigger for the installing user, InteractiveToken,
//     LeastPrivilege: no elevation, the operator's own session.
//   - RestartOnFailure 5 × PT1M is the restart-storm cap (the systemd
//     unit's StartLimitBurst=5/60s): a daemon that cannot start (missing
//     runtime, bad config) is retried five times, then left stopped for
//     tera status to see.
//   - ExecutionTimeLimit PT0S: never killed for running "too long".
//   - DisallowStartIfOnBatteries/StopIfGoingOnBatteries false: battery
//     is the governor's call (serve_on_battery), not the scheduler's.
//   - Priority 5 = NORMAL_PRIORITY_CLASS: same reasoning as the launchd
//     ProcessType Standard — QoS throttling starved token streaming;
//     being polite is the governor's job.
//   - Hidden: no entry in the operator's task list UI clutter; schtasks
//     /Query still shows it.
func renderTaskXML(binPath string, args []string, opts Options, userID string) string {
	if opts.LogPath != "" {
		args = append(append([]string{}, args...), "--log-file", opts.LogPath)
	}
	quoted := make([]string, 0, len(args))
	for _, a := range args {
		quoted = append(quoted, quoteArg(a))
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>Teraflock node daemon</Description>
    <URI>\%[1]s</URI>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>%[2]s</UserId>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>%[2]s</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings>
      <StopOnIdleEnd>false</StopOnIdleEnd>
      <RestartOnIdle>false</RestartOnIdle>
    </IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>true</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <DisallowStartOnRemoteAppSession>false</DisallowStartOnRemoteAppSession>
    <UseUnifiedSchedulingEngine>true</UseUnifiedSchedulingEngine>
    <WakeToRun>false</WakeToRun>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>5</Priority>
    <RestartOnFailure>
      <Interval>PT1M</Interval>
      <Count>5</Count>
    </RestartOnFailure>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>%[3]s</Command>
      <Arguments>%[4]s</Arguments>
      <WorkingDirectory>%[5]s</WorkingDirectory>
    </Exec>
  </Actions>
</Task>
`, taskName, xmlEscape(userID), xmlEscape(binPath), xmlEscape(strings.Join(quoted, " ")), xmlEscape(filepath.Dir(binPath)))
}

// quoteArg wraps an argument in double quotes when the Windows command
// line would otherwise split it (paths under Program Files, log paths
// with spaces in the user name). Embedded quotes are backslash-escaped
// per the MSVC/Go argv rules.
func quoteArg(a string) string {
	if a != "" && !strings.ContainsAny(a, " \t\"") {
		return a
	}
	return `"` + strings.ReplaceAll(a, `"`, `\"`) + `"`
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// utf16LEBOM encodes the definition the way schtasks /Query /XML exports
// it (and the declaration promises): UTF-16 little-endian with a BOM.
func utf16LEBOM(s string) []byte {
	u := utf16.Encode([]rune(s))
	out := make([]byte, 0, 2+2*len(u))
	out = append(out, 0xFF, 0xFE)
	for _, c := range u {
		out = append(out, byte(c), byte(c>>8))
	}
	return out
}

func schtasks(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "schtasks", args...).CombinedOutput()
	msg := strings.TrimSpace(string(out))
	if err != nil {
		if msg == "" {
			msg = err.Error()
		}
		return msg, fmt.Errorf("svc: schtasks %s: %w (%s)", args[0], err, msg)
	}
	return msg, nil
}

func (m *taskManager) Install(ctx context.Context, binPath string, args []string, opts Options) error {
	path, err := m.definitionPath()
	if err != nil {
		return err
	}
	userID, err := taskUser()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("svc: mkdir %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, utf16LEBOM(renderTaskXML(binPath, args, opts, userID)), 0o644); err != nil {
		return fmt.Errorf("svc: write task definition: %w", err)
	}
	// /F replaces an existing registration (re-running tera up after a
	// binary or log path change). The principal comes from the XML: /RU
	// without /RP would prompt for a password on stdin and hang tera up.
	if _, err := schtasks(ctx, "/Create", "/TN", taskName, "/XML", path, "/F"); err != nil {
		return err
	}
	return nil
}

func (m *taskManager) Uninstall(ctx context.Context) error {
	st, err := m.Status(ctx)
	if err != nil {
		return err
	}
	if st != StatusNotInstalled {
		_ = m.Stop(ctx)
		if _, err := schtasks(ctx, "/Delete", "/TN", taskName, "/F"); err != nil {
			return err
		}
	}
	path, err := m.definitionPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("svc: remove task definition: %w", err)
	}
	return nil
}

func (*taskManager) Start(ctx context.Context) error {
	if _, err := schtasks(ctx, "/Change", "/TN", taskName, "/ENABLE"); err != nil {
		return err
	}
	if _, err := schtasks(ctx, "/Run", "/TN", taskName); err != nil {
		return err
	}
	return nil
}

func (m *taskManager) Stop(ctx context.Context) error {
	st, err := m.Status(ctx)
	if err != nil {
		return err
	}
	if st == StatusNotInstalled {
		return errors.New("svc: flockd task is not installed (run `tera up`)")
	}
	if st == StatusRunning {
		if _, err := schtasks(ctx, "/End", "/TN", taskName); err != nil {
			return err
		}
	}
	if _, err := schtasks(ctx, "/Change", "/TN", taskName, "/DISABLE"); err != nil {
		return err
	}
	return nil
}

// statusScript prints the task's State enum name, or NotInstalled. The
// enum's ToString is invariant across display languages, unlike
// schtasks /Query output.
const statusScript = `$t = Get-ScheduledTask -TaskName '` + taskName + `' -ErrorAction SilentlyContinue; if ($t) { $t.State.ToString() } else { 'NotInstalled' }`

func (*taskManager) Status(ctx context.Context) (Status, error) {
	out, err := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", statusScript).Output()
	if err != nil {
		return StatusUnknown, fmt.Errorf("svc: Get-ScheduledTask: %w", err)
	}
	return parseTaskState(string(out)), nil
}

// parseTaskState maps the Microsoft.PowerShell.Cmdletization.GeneratedTypes
// .ScheduledTask.StateEnum name (Unknown, Disabled, Queued, Ready,
// Running) to a Status. Disabled/Ready/Queued are all "installed, not
// running"; Unknown is what the scheduler itself reports when it cannot
// tell, and is kept as such.
func parseTaskState(out string) Status {
	switch strings.TrimSpace(out) {
	case "Running":
		return StatusRunning
	case "Ready", "Disabled", "Queued":
		return StatusStopped
	case "NotInstalled":
		return StatusNotInstalled
	}
	return StatusUnknown
}
