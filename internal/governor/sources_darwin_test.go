//go:build darwin

package governor

import (
	"context"
	"testing"
	"time"
)

const ioregFixture = `
+-o IOHIDSystem  <class IOHIDSystem, id 0x100000456, registered, matched, active, busy 0 (2 ms), retain 12>
    {
      "HIDIdleTime" = 187543210000
      "HIDParameters" = {"stuff"=1}
    }
`

func TestParseHIDIdleTime(t *testing.T) {
	d, err := parseHIDIdleTime(ioregFixture)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Duration(187543210000) * time.Nanosecond
	if d != want {
		t.Errorf("idle = %v, want %v", d, want)
	}
	if _, err := parseHIDIdleTime("no idle here"); err == nil {
		t.Error("expected error for missing HIDIdleTime")
	}
}

func TestParsePmsetBatt(t *testing.T) {
	battery := "Now drawing from 'Battery Power'\n -InternalBattery-0 (id=123)\t87%; discharging; 4:32 remaining"
	ac := "Now drawing from 'AC Power'\n -InternalBattery-0 (id=123)\t100%; charged"
	if !parsePmsetBatt(battery).OnBattery {
		t.Error("battery fixture should report OnBattery")
	}
	if parsePmsetBatt(ac).OnBattery {
		t.Error("AC fixture should not report OnBattery")
	}
}

func TestDarwinIdleSourceLive(t *testing.T) {
	// Live smoke test on macOS runners: must return without error.
	d, err := NewPlatformIdleSource().IdleFor(context.Background())
	if err != nil {
		t.Skipf("ioreg unavailable: %v", err)
	}
	if d < 0 {
		t.Errorf("negative idle %v", d)
	}
}
