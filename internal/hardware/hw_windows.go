//go:build windows

package hardware

import (
	"context"
	"errors"
	"os"

	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
	"golang.org/x/sys/windows"
)

// ErrWindowsDetectionStub marks probes not yet implemented on Windows.
// TODO(windows): WMI (Win32_VideoController) + NVML for GPU/VRAM,
// GlobalMemoryStatusEx for RAM detail, registry for CPU model.
var ErrWindowsDetectionStub = errors.New("hardware: windows detection is a stub")

func detectPlatform(ctx context.Context, p *typesv1.CapabilityProfile) error {
	// Minimal viable detection so the daemon runs on Windows: RAM via
	// GlobalMemoryStatusEx-equivalent is stubbed; GPUs left empty so the
	// caller applies the CPU-only fallback.
	p.CpuModel = os.Getenv("PROCESSOR_IDENTIFIER")
	return nil
}

func diskFreeMB(path string) (uint64, error) {
	var free, total, avail uint64
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
		return 0, err
	}
	return avail / (1024 * 1024), nil
}
