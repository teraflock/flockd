//go:build darwin

package memory

import (
	"fmt"
	"syscall"
	"unsafe"
)

// proc_pid_rusage(pid, RUSAGE_INFO_V0, &info) is the libproc call behind
// Activity Monitor's memory column, `footprint`, `vmmap -summary` and
// modern `top`. It is not a raw syscall, so unix/syscall cannot expose it —
// but it does not need cgo either: libSystem functions are reachable
// through the same dynamic-import trampoline golang.org/x/sys/unix uses
// for every darwin libc call (the runtime exports syscall.syscall for
// exactly this, see runtime/sys_darwin.go). Release builds are
// CGO_ENABLED=0 (.goreleaser.yaml) and stay that way.
//
// Alternatives measured on an M3 Max against a 14 GB llama-server:
// `ps -o rss=` reported 1.5 MB (useless for an mmap-heavy process),
// `top -l 1 -pid` agreed but takes ~0.7 s per sample and rounds to whole
// GB above 10 GB, `vmmap -summary` agreed but takes ~6 s.

//go:cgo_import_dynamic libc_proc_pid_rusage proc_pid_rusage "/usr/lib/libSystem.B.dylib"

var libc_proc_pid_rusage_trampoline_addr uintptr

//go:linkname syscall_syscall syscall.syscall
func syscall_syscall(fn, a1, a2, a3 uintptr) (r1, r2, err uintptr)

const rusageInfoV0 = 0

// rusageInfoV0 layout (sys/resource.h): uuid[16] then 10 uint64 fields;
// ri_phys_footprint is the 8th of them.
type rusageInfo struct {
	UUID           [16]byte
	UserTime       uint64
	SystemTime     uint64
	PkgIdleWkups   uint64
	InterruptWkups uint64
	Pageins        uint64
	WiredSize      uint64
	ResidentSize   uint64
	PhysFootprint  uint64
	ProcStart      uint64
	ProcExit       uint64
}

func processFootprintBytes(pid int) (uint64, error) {
	var info rusageInfo
	r1, _, e := syscall_syscall(libc_proc_pid_rusage_trampoline_addr, uintptr(pid), rusageInfoV0, uintptr(unsafe.Pointer(&info)))
	if int32(r1) != 0 || e != 0 {
		if e == 0 {
			return 0, fmt.Errorf("memory: proc_pid_rusage(%d) failed", pid)
		}
		return 0, fmt.Errorf("memory: proc_pid_rusage(%d): %w", pid, syscall.Errno(e))
	}
	return info.PhysFootprint, nil
}
