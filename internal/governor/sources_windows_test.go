//go:build windows

package governor

import (
	"context"
	"testing"
)

func TestPowerFromSystemStatus(t *testing.T) {
	cases := []struct {
		name        string
		ac, battery uint8
		onBattery   bool
	}{
		{"laptop unplugged", acLineOffline, 1, true},
		{"laptop unplugged, low battery", acLineOffline, 4, true},
		{"laptop plugged in", acLineOnline, 8, false},
		{"plugged in, charging", acLineOnline, 8 | 1, false},
		{"desktop: unknown line, no battery", acLineUnknown, batteryFlagNoBattery, false},
		{"desktop: unknown line, flag unknown", acLineUnknown, 255, false},
		// Offline but no battery at all: a UPS-less desktop reporting
		// oddly; never pause on that.
		{"offline but no system battery", acLineOffline, batteryFlagNoBattery, false},
	}
	for _, c := range cases {
		got := powerFromSystemStatus(c.ac, c.battery)
		if got.OnBattery != c.onBattery {
			t.Errorf("%s: OnBattery = %v, want %v", c.name, got.OnBattery, c.onBattery)
		}
		if got.TempCelsius != 0 {
			t.Errorf("%s: TempCelsius = %v, want 0 (unknown on Windows)", c.name, got.TempCelsius)
		}
	}
}

// Live: the real call on the windows-latest runner (a VM without a
// battery, which must read as mains power so the node serves).
func TestWindowsPowerSourceLive(t *testing.T) {
	ps, err := NewPlatformPowerSource().Status(context.Background())
	if err != nil {
		t.Fatalf("GetSystemPowerStatus: %v", err)
	}
	if ps.OnBattery {
		t.Log("runner reports battery power; not asserting either way on unknown hardware")
	}
}

// The idle source shipped in v0.5.0 had no live test; exercise the real
// GetLastInputInfo call here too since the lane exists.
func TestWindowsIdleSourceLive(t *testing.T) {
	if _, err := NewPlatformIdleSource().IdleFor(context.Background()); err != nil {
		t.Fatalf("GetLastInputInfo: %v", err)
	}
}
