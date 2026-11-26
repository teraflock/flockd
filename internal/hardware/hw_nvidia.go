//go:build linux || windows

package hardware

import (
	"context"
	"log/slog"
	"os/exec"
	"runtime"

	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
)

// detectNvidia shells out to the first nvidia-smi found among candidates
// (a bare name is resolved on PATH, an absolute path is tried as-is).
// Shared by Linux and Windows: the CSV query is identical on both and the
// tool ships with every NVIDIA driver, which keeps flockd cgo-free (no
// NVML binding, SPEC §A1.3). Nil when no tool or no GPU.
//
// Accel is cuda12 — the runtime artifact lanes are keyed on it for both
// linux/amd64 and windows/amd64 — unless the driver reports that no GPU on
// the box can run the CUDA build (nvidia.go), which is logged once per
// affected GPU.
func detectNvidia(ctx context.Context, candidates ...string) []*typesv1.GpuInfo {
	rows, ok := runNvidiaSMI(ctx, candidates...)
	if !ok {
		return nil
	}
	gpus, warnings := nvidiaGPUInfos(rows, vulkanVendors()["nvidia"], runtime.GOOS)
	for _, w := range warnings {
		slog.Default().Warn(w)
	}
	return gpus
}

// runNvidiaSMI queries the first nvidia-smi found among candidates. ok is
// false when none of them resolves to a tool.
func runNvidiaSMI(ctx context.Context, candidates ...string) (rows []nvidiaGPU, ok bool) {
	for _, c := range candidates {
		path, err := exec.LookPath(c)
		if err != nil {
			continue
		}
		return queryNvidiaSMI(func(fields string) ([]byte, error) {
			return exec.CommandContext(ctx, path,
				"--query-gpu="+fields,
				"--format=csv,noheader,nounits").Output()
		}), true
	}
	return nil, false
}

// ProbeNVIDIA asks the NVIDIA driver about its GPUs for diagnostics: the
// driver version, the compute capability when the driver reports it, and
// the explanation for a card the published CUDA build cannot run on. It
// logs nothing. Nil when there is no NVIDIA driver tool or no GPU.
func ProbeNVIDIA(ctx context.Context) []NVIDIAGPU {
	rows, _ := runNvidiaSMI(ctx, nvidiaSMICandidates()...)
	return nvidiaReport(rows, vulkanVendors()["nvidia"], runtime.GOOS)
}
