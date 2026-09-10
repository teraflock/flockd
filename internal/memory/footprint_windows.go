//go:build windows

package memory

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows: GetProcessMemoryInfo (psapi) with PROCESS_MEMORY_COUNTERS_EX,
// cgo-free through the same lazy-DLL route hardware/hw_windows.go uses
// for GlobalMemoryStatusEx (x/sys/windows wraps OpenProcess but not this
// call). The footprint is the larger of two counters:
//
//   - PrivateUsage: the commit charge — heap, KV cache, compute buffers,
//     and the weights when llama-server runs with --no-mmap. It is what
//     Task Manager's "Commit size" shows and never shrinks under memory
//     pressure.
//   - WorkingSetSize: resident pages including file-backed ones, which is
//     where mmap'd GGUF weights show up (they are not private). It is
//     what Task Manager's "Memory (active private working set)" column
//     under-reports, and the kernel trims it under pressure.
//
// Taking the max keeps the mmap case measured while staying on the
// conservative side for admission (under-fill rather than OOM), matching
// the phys_footprint / Pss choice on the other platforms.
var (
	psapi                    = windows.NewLazySystemDLL("psapi.dll")
	procGetProcessMemoryInfo = psapi.NewProc("GetProcessMemoryInfo")
)

// processMemoryCountersEx mirrors PROCESS_MEMORY_COUNTERS_EX (psapi.h).
// cb must be set to the struct size before the call.
type processMemoryCountersEx struct {
	cb                         uint32
	pageFaultCount             uint32
	peakWorkingSetSize         uintptr
	workingSetSize             uintptr
	quotaPeakPagedPoolUsage    uintptr
	quotaPagedPoolUsage        uintptr
	quotaPeakNonPagedPoolUsage uintptr
	quotaNonPagedPoolUsage     uintptr
	pagefileUsage              uintptr
	peakPagefileUsage          uintptr
	privateUsage               uintptr
}

func processFootprintBytes(pid int) (uint64, error) {
	if pid <= 0 {
		return 0, fmt.Errorf("memory: invalid pid %d", pid)
	}
	if err := procGetProcessMemoryInfo.Find(); err != nil {
		return 0, fmt.Errorf("memory: %w", err)
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0, fmt.Errorf("memory: OpenProcess(%d): %w", pid, err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var pmc processMemoryCountersEx
	pmc.cb = uint32(unsafe.Sizeof(pmc))
	r, _, e := procGetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&pmc)), uintptr(pmc.cb))
	if r == 0 {
		return 0, fmt.Errorf("memory: GetProcessMemoryInfo(%d): %w", pid, e)
	}
	return uint64(max(pmc.privateUsage, pmc.workingSetSize)), nil
}
