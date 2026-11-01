//go:build windows

package governor

import (
	"fmt"
	"log/slog"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// NewPlatformSleepInhibitor returns the Windows inhibitor: a power
// request object (kernel32 PowerCreateRequest) set to
// PowerRequestSystemRequired. That is the per-process, reason-tagged
// form of "keep the system awake": it stops the idle sleep timer, never
// the display (that would be PowerRequestDisplayRequired, which flockd
// never sets), and `powercfg /requests` lists it under SYSTEM with the
// reason string. SetThreadExecutionState is deliberately not used: it is
// per OS thread, and a Go goroutine is not pinned to one.
func NewPlatformSleepInhibitor(log *slog.Logger) SleepInhibitor {
	return &windowsInhibitor{log: log}
}

var (
	procPowerCreateRequest = kernel32.NewProc("PowerCreateRequest")
	procPowerSetRequest    = kernel32.NewProc("PowerSetRequest")
	procPowerClearRequest  = kernel32.NewProc("PowerClearRequest")
)

// POWER_REQUEST_TYPE (winnt.h): SystemRequired is 1. DisplayRequired (0)
// is never set here, by policy.
const powerRequestSystemRequired = 1

// REASON_CONTEXT (minwinbase.h) in its simple-string form.
const (
	powerRequestContextVersion      = 0
	powerRequestContextSimpleString = 1
)

// reasonContext mirrors REASON_CONTEXT: Version, Flags, then a union whose
// largest member (Detailed: HMODULE, ULONG, LPWSTR*) is three words. The
// simple-string member is the first word; the padding keeps the struct
// the size the API expects.
type reasonContext struct {
	Version uint32
	Flags   uint32
	Simple  *uint16
	_       [2]uintptr
}

type windowsInhibitor struct {
	log *slog.Logger

	mu     sync.Mutex
	handle windows.Handle
	// reason keeps the UTF-16 string reachable while the request lives.
	reason *uint16
}

func (w *windowsInhibitor) Hold(reason string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.handle != 0 {
		return nil
	}
	if err := procPowerCreateRequest.Find(); err != nil {
		return fmt.Errorf("%w (%v)", ErrNoSleepInhibitor, err)
	}
	why, err := windows.UTF16PtrFromString(reason)
	if err != nil {
		return fmt.Errorf("governor: power request reason: %w", err)
	}
	rc := reasonContext{Version: powerRequestContextVersion, Flags: powerRequestContextSimpleString, Simple: why}
	h, _, e := procPowerCreateRequest.Call(uintptr(unsafe.Pointer(&rc)))
	if windows.Handle(h) == windows.InvalidHandle || h == 0 {
		return fmt.Errorf("governor: PowerCreateRequest: %w", e)
	}
	if r, _, e := procPowerSetRequest.Call(h, powerRequestSystemRequired); r == 0 {
		windows.CloseHandle(windows.Handle(h))
		return fmt.Errorf("governor: PowerSetRequest(SystemRequired): %w", e)
	}
	w.handle, w.reason = windows.Handle(h), why
	return nil
}

func (w *windowsInhibitor) Release() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.handle == 0 {
		return nil
	}
	h := w.handle
	w.handle, w.reason = 0, nil
	var firstErr error
	if r, _, e := procPowerClearRequest.Call(uintptr(h), powerRequestSystemRequired); r == 0 {
		firstErr = fmt.Errorf("governor: PowerClearRequest: %w", e)
	}
	// Closing the handle drops every request on it even if Clear failed.
	if err := windows.CloseHandle(h); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("governor: CloseHandle(power request): %w", err)
	}
	return firstErr
}

// Held reports whether a request object is live (tests).
func (w *windowsInhibitor) Held() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.handle != 0
}
