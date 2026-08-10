//go:build linux

package governor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// NewPlatformIdleSource returns the Linux idle source.
//
// TODO(logind): query org.freedesktop.login1 IdleHint/IdleSinceHint over
// DBus for desktop sessions. Headless boxes have no idle signal at all;
// the governor treats the error as "assume idle" (with a one-time warning),
// which is the right default for servers.
func NewPlatformIdleSource() IdleSource { return linuxIdleSource{} }

type linuxIdleSource struct{}

func (linuxIdleSource) IdleFor(context.Context) (time.Duration, error) {
	return 0, fmt.Errorf("%w (linux logind DBus integration pending)", ErrNoIdleSource)
}

// NewPlatformPowerSource reads /sys/class/power_supply and thermal zones.
func NewPlatformPowerSource() PowerSource { return linuxPowerSource{root: "/sys"} }

type linuxPowerSource struct{ root string }

func (s linuxPowerSource) Status(context.Context) (PowerStatus, error) {
	ps := PowerStatus{}
	// AC online? Any ACAD/AC* supply with online=1 means not on battery.
	supplies, _ := filepath.Glob(filepath.Join(s.root, "class/power_supply/*"))
	onAC := len(supplies) == 0 // desktops without a battery: treat as AC
	for _, sup := range supplies {
		typ, _ := os.ReadFile(filepath.Join(sup, "type"))
		if strings.TrimSpace(string(typ)) == "Mains" {
			online, _ := os.ReadFile(filepath.Join(sup, "online"))
			if strings.TrimSpace(string(online)) == "1" {
				onAC = true
			}
		}
	}
	ps.OnBattery = !onAC

	// Best-effort temperature from the hottest thermal zone.
	zones, _ := filepath.Glob(filepath.Join(s.root, "class/thermal/thermal_zone*/temp"))
	for _, z := range zones {
		raw, err := os.ReadFile(z)
		if err != nil {
			continue
		}
		var milli int64
		if _, err := fmt.Sscanf(strings.TrimSpace(string(raw)), "%d", &milli); err == nil {
			if c := float64(milli) / 1000; c > ps.TempCelsius {
				ps.TempCelsius = c
			}
		}
	}
	return ps, nil
}
