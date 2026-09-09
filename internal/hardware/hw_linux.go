//go:build linux

package hardware

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
	"golang.org/x/sys/unix"
)

func detectPlatform(ctx context.Context, p *typesv1.CapabilityProfile) error {
	p.RamTotalMb = readMemTotalMB("/proc/meminfo")
	p.CpuModel = readCPUModel("/proc/cpuinfo")

	if gpus := detectNvidia(ctx, "nvidia-smi"); len(gpus) > 0 {
		p.Gpus = gpus
		return nil
	}
	// TODO(rocm): parse rocm-smi / /sys/class/drm for AMD GPUs. CPU-only
	// fallback is applied by the caller when no GPUs are found.
	return nil
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
