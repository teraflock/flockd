//go:build windows

package governor

import "testing"

// Live on the windows-latest runner: a power request is created, set and
// cleared. There is no API to read a request back, so this covers that
// the calls succeed and the handle lifecycle is right; `powercfg
// /requests` is the manual check.
func TestWindowsSleepInhibitorLive(t *testing.T) {
	w := NewPlatformSleepInhibitor(quietLogger()).(*windowsInhibitor)
	if err := w.Hold(AwakeReasonServing); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if err := w.Hold(AwakeReasonRequest); err != nil { // idempotent
		t.Fatalf("second Hold: %v", err)
	}
	if !w.Held() {
		t.Fatal("not held after Hold")
	}
	if err := w.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if w.Held() {
		t.Fatal("held after Release")
	}
	if err := w.Release(); err != nil { // idempotent
		t.Fatalf("second Release: %v", err)
	}
	if err := w.Hold(AwakeReasonServing); err != nil {
		t.Fatalf("Hold after Release: %v", err)
	}
	if err := w.Release(); err != nil {
		t.Fatalf("final Release: %v", err)
	}
}
