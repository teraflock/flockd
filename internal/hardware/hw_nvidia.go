//go:build linux || windows

package hardware

import (
	"context"
	"os/exec"
	"strconv"
	"strings"

	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
)

// detectNvidia shells out to the first nvidia-smi found among candidates
// (a bare name is resolved on PATH, an absolute path is tried as-is).
// Shared by Linux and Windows: the CSV query is identical on both and the
// tool ships with every NVIDIA driver, which keeps flockd cgo-free (no
// NVML binding, SPEC §A1.3). Nil when no tool or no GPU.
func detectNvidia(ctx context.Context, candidates ...string) []*typesv1.GpuInfo {
	for _, c := range candidates {
		path, err := exec.LookPath(c)
		if err != nil {
			continue
		}
		out, err := exec.CommandContext(ctx, path,
			"--query-gpu=name,memory.total,driver_version",
			"--format=csv,noheader,nounits").Output()
		if err != nil {
			return nil
		}
		return parseNvidiaSMI(string(out))
	}
	return nil
}

// parseNvidiaSMI reads one `name, memory.total, driver_version` line per
// GPU. Accel is cuda12: the runtime artifact lanes are keyed on it for
// both linux/amd64 and windows/amd64.
func parseNvidiaSMI(out string) []*typesv1.GpuInfo {
	var gpus []*typesv1.GpuInfo
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		parts := strings.Split(line, ",")
		if len(parts) < 3 {
			continue
		}
		memMB, _ := strconv.ParseUint(strings.TrimSpace(parts[1]), 10, 64)
		gpus = append(gpus, &typesv1.GpuInfo{
			Vendor:        "nvidia",
			Model:         strings.TrimSpace(parts[0]),
			VramMb:        memMB,
			DriverVersion: strings.TrimSpace(parts[2]),
			Accel:         "cuda12",
		})
	}
	return gpus
}
