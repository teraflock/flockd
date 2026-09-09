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

// NewPlatformPowerSource returns the Windows power source
// (kernel32 GetSystemPowerStatus).
//
// Temperature stays unknown (TempCelsius 0): the WMI
// MSAcpi_ThermalZoneTemperature class is absent or static on most consumer
// boards and needs an elevated token, so max_temp_celsius is inert on
// Windows. NVIDIA GPU temperature via nvidia-smi is a possible follow-up
// next to flockd#16.
func NewPlatformPowerSource() PowerSource { return windowsPowerSource{} }

var procGetSystemPowerStatus = kernel32.NewProc("GetSystemPowerStatus")

// systemPowerStatus mirrors SYSTEM_POWER_STATUS (winbase.h).
type systemPowerStatus struct {
	ACLineStatus        uint8
	BatteryFlag         uint8
	BatteryLifePercent  uint8
	SystemStatusFlag    uint8
	BatteryLifeTime     uint32
	BatteryFullLifeTime uint32
}

// ACLineStatus values.
const (
	acLineOffline = 0   // running on battery
	acLineOnline  = 1   // mains power
	acLineUnknown = 255 // no battery driver / virtual machine
)

// batteryFlagNoBattery is the BatteryFlag bit for "no system battery".
const batteryFlagNoBattery = 128

type windowsPowerSource struct{}

func (windowsPowerSource) Status(context.Context) (PowerStatus, error) {
	if err := procGetSystemPowerStatus.Find(); err != nil {
		return PowerStatus{}, fmt.Errorf("governor: GetSystemPowerStatus: %w", err)
	}
	var sps systemPowerStatus
	r, _, e := procGetSystemPowerStatus.Call(uintptr(unsafe.Pointer(&sps)))
	if r == 0 {
		return PowerStatus{}, fmt.Errorf("governor: GetSystemPowerStatus: %w", e)
	}
	return powerFromSystemStatus(sps.ACLineStatus, sps.BatteryFlag), nil
}

// powerFromSystemStatus is the pure mapping: on battery only when the AC
// line is reported offline on a machine that has a battery. Unknown (255,
// desktops and VMs without a battery driver) and "no system battery"
// both read as mains power, so desktop nodes serve by default.
func powerFromSystemStatus(acLineStatus, batteryFlag uint8) PowerStatus {
	onBattery := acLineStatus == acLineOffline && batteryFlag&batteryFlagNoBattery == 0
	return PowerStatus{OnBattery: onBattery}
}
