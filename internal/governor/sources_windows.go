//go:build windows

package governor

import (
	"context"
	"fmt"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows idle: user32 GetLastInputInfo gives the tick count of the last
// keyboard/mouse input in the calling session; against the current tick
// count that is the idle time. Loaded lazily from the system DLL — no
// cgo, and a missing export fails the call, not the process.
//
// Caveat: the value is per session. The daemon runs in the operator's
// session (tera up installs a per-user service, and the desktop app
// starts it); run as a session-0 service it would see no input at all
// and count the machine as idle since boot.
var (
	user32               = windows.NewLazySystemDLL("user32.dll")
	kernel32             = windows.NewLazySystemDLL("kernel32.dll")
	procGetLastInputInfo = user32.NewProc("GetLastInputInfo")
	procGetTickCount64   = kernel32.NewProc("GetTickCount64")
)

// lastInputInfo mirrors LASTINPUTINFO (winuser.h).
type lastInputInfo struct {
	cbSize uint32
	dwTime uint32
}

// NewPlatformIdleSource returns the Windows idle source (GetLastInputInfo).
func NewPlatformIdleSource() IdleSource { return windowsIdleSource{} }

type windowsIdleSource struct{}

func (windowsIdleSource) IdleFor(context.Context) (time.Duration, error) {
	if err := procGetLastInputInfo.Find(); err != nil {
		return 0, fmt.Errorf("%w (%v)", ErrNoIdleSource, err)
	}
	var lii lastInputInfo
	lii.cbSize = uint32(unsafe.Sizeof(lii))
	r, _, e := procGetLastInputInfo.Call(uintptr(unsafe.Pointer(&lii)))
	if r == 0 {
		return 0, fmt.Errorf("governor: GetLastInputInfo: %w", e)
	}
	now, _, _ := procGetTickCount64.Call()
	return idleFromTicks(uint64(now), lii.dwTime), nil
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
