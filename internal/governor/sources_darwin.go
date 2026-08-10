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
// Screen-lock detection via CGSessionCopyCurrentDictionary is a TODO
// (requires a cgo shim; ioreg gets us the important signal).
type darwinIdleSource struct{}

var hidIdleRe = regexp.MustCompile(`"HIDIdleTime"\s*=\s*(\d+)`)

func (darwinIdleSource) IdleFor(ctx context.Context) (time.Duration, error) {
	out, err := exec.CommandContext(ctx, "ioreg", "-c", "IOHIDSystem", "-d", "4").Output()
	if err != nil {
		return 0, fmt.Errorf("governor: ioreg: %w", err)
	}
	return parseHIDIdleTime(string(out))
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
