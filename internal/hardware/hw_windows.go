//go:build windows

package hardware

import (
	"context"
	"os"

	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
	"golang.org/x/sys/windows"
)

// detectPlatform is the minimal Windows probe: CPU model from the
// environment, GPUs left empty so the caller applies the CPU-only
// fallback. RAM, CPU model and GPU/VRAM detection (WMI + NVML) are the
// Windows epic's job — flockd#29.
func detectPlatform(_ context.Context, p *typesv1.CapabilityProfile) error {
	p.CpuModel = os.Getenv("PROCESSOR_IDENTIFIER")
	return nil
}

func diskFreeBytes(path string) (uint64, error) {
	var free, total, avail uint64
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
		return 0, err
	}
	return avail, nil
}
