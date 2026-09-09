//go:build darwin

package governor

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// NewPlatformIdleSource returns the macOS idle source (ioreg HIDIdleTime).
func NewPlatformIdleSource() IdleSource { return &darwinIdleSource{} }

// NewPlatformPowerSource returns the macOS power source (pmset).
func NewPlatformPowerSource() PowerSource { return &darwinPowerSource{} }

// darwinIdleSource parses HIDIdleTime (nanoseconds since last HID input)
// from the IOKit registry. This covers keyboard/mouse/trackpad activity.
// A locked screen counts as idle too: loginwindow mirrors the lock state
// into the registry root as IOConsoleLocked (and, per console user,
// CGSSessionScreenIsLocked in IOConsoleUsers), which is the same fact
// CGSessionCopyCurrentDictionary reports — read via `ioreg -n Root -d 1`
// (~20ms) instead of a cgo shim against CoreGraphics, so the release
// build stays CGO_ENABLED=0.
type darwinIdleSource struct{}

var (
	hidIdleRe       = regexp.MustCompile(`"HIDIdleTime"\s*=\s*(\d+)`)
	consoleLockedRe = regexp.MustCompile(`"(?:IOConsoleLocked|CGSSessionScreenIsLocked)"\s*=\s*(?:Yes|1|true)\b`)
)

func (darwinIdleSource) IdleFor(ctx context.Context) (time.Duration, error) {
	out, err := exec.CommandContext(ctx, "ioreg", "-c", "IOHIDSystem", "-d", "4").Output()
	if err != nil {
		return 0, fmt.Errorf("governor: ioreg: %w", err)
	}
	d, err := parseHIDIdleTime(string(out))
	if err != nil {
		return 0, err
	}
	// The lock probe is best-effort on top of the input signal: if it
	// fails, the HID idle time alone still answers.
	if root, err := exec.CommandContext(ctx, "ioreg", "-n", "Root", "-d", "1").Output(); err == nil && parseConsoleLocked(string(root)) && d < idleSinceUnknown {
		d = idleSinceUnknown
	}
	return d, nil
}

// parseConsoleLocked reads the `ioreg -n Root -d 1` property dump: the
// screen is locked when IOConsoleLocked = Yes on the root, or a console
// user carries CGSSessionScreenIsLocked = Yes. Absent keys mean unlocked
// (both are only set once loginwindow has locked at least once).
func parseConsoleLocked(out string) bool {
	return consoleLockedRe.MatchString(out)
}

func parseHIDIdleTime(out string) (time.Duration, error) {
	m := hidIdleRe.FindStringSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("governor: HIDIdleTime not found in ioreg output")
	}
	ns, err := strconv.ParseUint(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("governor: parse HIDIdleTime: %w", err)
	}
	return time.Duration(ns) * time.Nanosecond, nil
}

// darwinPowerSource parses `pmset -g batt`. GPU temperature requires SMC
// access (powermetrics needs root); reported as unknown (0) for now —
// TODO(smc): read temperature via IOKit SMC keys.
type darwinPowerSource struct{}

func (darwinPowerSource) Status(ctx context.Context) (PowerStatus, error) {
	out, err := exec.CommandContext(ctx, "pmset", "-g", "batt").Output()
	if err != nil {
		return PowerStatus{}, fmt.Errorf("governor: pmset: %w", err)
	}
	return parsePmsetBatt(string(out)), nil
}

func parsePmsetBatt(out string) PowerStatus {
	return PowerStatus{
		OnBattery: strings.Contains(out, "'Battery Power'"),
	}
}
