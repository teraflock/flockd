//go:build darwin

package governor

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
)

// NewPlatformSleepInhibitor returns the macOS inhibitor: a `caffeinate -i
// -w <pid>` child. -i is the IOPMAssertion that prevents idle *system*
// sleep (PreventUserIdleSystemSleep) and nothing else — the display
// sleeps on its own schedule, the lid still sleeps the machine, and
// -w ties the assertion to flockd's own pid so a crashed daemon cannot
// leave it behind. `pmset -g assertions` lists it under caffeinate with
// flockd's pid. No cgo: the release build stays CGO_ENABLED=0, same as
// the idle and power sources here.
func NewPlatformSleepInhibitor(log *slog.Logger) SleepInhibitor {
	return &execInhibitor{log: log, spawn: caffeinate}
}

// caffeinate builds the macOS child. reason is not shown by pmset (it
// prints the process name), so it only goes to the log.
func caffeinate(ctx context.Context, _ string) *exec.Cmd {
	return exec.CommandContext(ctx, "caffeinate", "-i", "-w", strconv.Itoa(os.Getpid()))
}
