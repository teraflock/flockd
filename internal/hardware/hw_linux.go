//go:build linux

package hardware

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
	"golang.org/x/sys/unix"
)

func detectPlatform(ctx context.Context, p *typesv1.CapabilityProfile) error {
	p.RamTotalMb = readMemTotalMB("/proc/meminfo")
	p.CpuModel = readCPUModel("/proc/cpuinfo")

	// NVIDIA first, AMD only when nvidia-smi found nothing (flockd#20).
	// CPU-only fallback is applied by the caller when no GPUs are found.
	p.Gpus = pickLinuxGPUs(detectNvidia(ctx, "nvidia-smi"), func() []*typesv1.GpuInfo {
		return detectAMD(ctx, "/")
	})
	return nil
}

// rocmSMITimeout bounds the one rocm-smi call at detection; a wedged ROCm
// stack must not stall boot (sysfs already gave us the GPU).
const rocmSMITimeout = 5 * time.Second

// rocmSMIArgs query product names, VRAM totals, bus ids and the driver
// version in one call. Every flag has existed since ROCm 3.x.
var rocmSMIArgs = []string{"--showproductname", "--showmeminfo", "vram", "--showbus", "--showdriverversion", "--json"}

// detectAMD reads the amdgpu sysfs tree under root and enriches it with
// rocm-smi when that is on PATH. See amd.go for the parsers.
func detectAMD(ctx context.Context, root string) []*typesv1.GpuInfo {
	sys := amdSysfsGPUs(root)
	if unbound := amdUnboundPCI(root, sys); len(unbound) > 0 {
		slog.Default().Info("amd display device without amdgpu driver bound; not usable for inference",
			"pci", strings.Join(unbound, ","))
	}
	if len(sys) == 0 {
		return nil
	}
	var smi rocmSMIInfo
	if out, err := runRocmSMI(ctx); err == nil {
		if parsed, perr := parseRocmSMI(out); perr == nil {
			smi = parsed
		} else {
			slog.Default().Debug("rocm-smi output not parsed; using sysfs only", "err", perr)
		}
	}
	return buildAMDGPUs(root, sys, smi, kernelRelease())
}

func runRocmSMI(ctx context.Context) ([]byte, error) {
	path, err := exec.LookPath("rocm-smi")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, rocmSMITimeout)
	defer cancel()
	return exec.CommandContext(ctx, path, rocmSMIArgs...).Output()
}

func kernelRelease() string {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return ""
	}
	return unix.ByteSliceToString(u.Release[:])
}

func readMemTotalMB(path string) uint64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "MemTotal:"); ok {
			fields := strings.Fields(v)
			if len(fields) >= 1 {
				kb, _ := strconv.ParseUint(fields[0], 10, 64)
				return kb / 1024
			}
		}
	}
	return 0
}

func readCPUModel(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "model name") {
			if _, after, ok := strings.Cut(sc.Text(), ":"); ok {
				return strings.TrimSpace(after)
			}
		}
	}
	return ""
}

func diskFreeBytes(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
