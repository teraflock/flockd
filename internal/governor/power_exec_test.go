//go:build darwin || linux

package governor

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

// The exec-backed inhibitor holds exactly while its child lives: Hold
// starts it, Release kills it and waits, both idempotent. Driven with a
// plain `sleep` so it runs anywhere.
func TestExecInhibitorHoldRelease(t *testing.T) {
	e := &execInhibitor{log: quietLogger(), spawn: func(ctx context.Context, _ string) *exec.Cmd {
		return exec.CommandContext(ctx, "sleep", "60")
	}}
	if e.Running() {
		t.Fatal("running before Hold")
	}
	if err := e.Hold("test"); err != nil {
		t.Fatal(err)
	}
	if err := e.Hold("test again"); err != nil {
		t.Fatal(err)
	}
	if !e.Running() {
		t.Fatal("not running after Hold")
	}
	if err := e.Release(); err != nil {
		t.Fatal(err)
	}
	if e.Running() {
		t.Fatal("running after Release")
	}
	if err := e.Release(); err != nil {
		t.Fatal(err)
	}
	// Hold after Release starts a fresh child.
	if err := e.Hold("test"); err != nil {
		t.Fatal(err)
	}
	if !e.Running() {
		t.Fatal("not running after second Hold")
	}
	if err := e.Release(); err != nil {
		t.Fatal(err)
	}
}

// A helper that dies on its own is noticed: Running turns false, and the
// next Hold starts another.
func TestExecInhibitorHelperExit(t *testing.T) {
	e := &execInhibitor{log: quietLogger(), spawn: func(ctx context.Context, _ string) *exec.Cmd {
		return exec.CommandContext(ctx, "true")
	}}
	if err := e.Hold("test"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for e.Running() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if e.Running() {
		t.Fatal("helper exit not observed")
	}
	if err := e.Release(); err != nil {
		t.Fatal(err)
	}
}

// Live: the platform helper (caffeinate / systemd-inhibit) starts and
// stops. Skipped where the helper is missing or refuses (a container
// without logind): the governor treats that as "no inhibitor", logged
// once.
func TestPlatformSleepInhibitorLive(t *testing.T) {
	i := NewPlatformSleepInhibitor(quietLogger()).(*execInhibitor)
	if _, err := exec.LookPath(i.spawn(context.Background(), "probe").Path); err != nil {
		t.Skipf("helper not installed: %v", err)
	}
	if err := i.Hold(AwakeReasonServing); err != nil {
		t.Skipf("helper refused: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if !i.Running() {
		t.Skip("helper exited at once (no logind / no power management here)")
	}
	if err := i.Release(); err != nil {
		t.Fatal(err)
	}
	if i.Running() {
		t.Fatal("helper still running after Release")
	}
}
