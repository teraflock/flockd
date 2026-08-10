//go:build windows

package governor

import (
	"context"
	"fmt"
	"time"
)

// NewPlatformIdleSource returns the Windows idle source.
//
// TODO(windows): GetLastInputInfo via golang.org/x/sys/windows for input
// idle; WTSRegisterSessionNotification for lock events. Until then the
// governor assumes idle (with a one-time warning).
func NewPlatformIdleSource() IdleSource { return windowsIdleSource{} }

type windowsIdleSource struct{}

func (windowsIdleSource) IdleFor(context.Context) (time.Duration, error) {
	return 0, fmt.Errorf("%w (windows GetLastInputInfo integration pending)", ErrNoIdleSource)
}

// NewPlatformPowerSource returns the Windows power source.
//
// TODO(windows): GetSystemPowerStatus for battery; WMI MSAcpi_ThermalZone
// for temperature. Stub reports AC power / unknown temperature so desktop
// nodes serve by default.
func NewPlatformPowerSource() PowerSource { return windowsPowerSource{} }

type windowsPowerSource struct{}

func (windowsPowerSource) Status(context.Context) (PowerStatus, error) {
	return PowerStatus{OnBattery: false}, nil
}
