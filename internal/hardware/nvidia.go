package hardware

// NVIDIA GPU parsing and the CUDA compute-capability gate. Pure functions
// (no exec, no build tag) so they run under test on every OS; hw_nvidia.go
// wires them to the real nvidia-smi on Linux and Windows.

import (
	"fmt"
	"strconv"
	"strings"

	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
)

const (
	// nvidiaSMIFields is the query every nvidia-smi understands.
	nvidiaSMIFields = "name,memory.total,driver_version"
	// nvidiaSMIFieldsWithCap adds the CUDA compute capability ("8.9").
	// Older drivers do not know the field and fail the whole query, so it
	// is only ever tried first, never relied on (queryNvidiaSMI).
	nvidiaSMIFieldsWithCap = nvidiaSMIFields + ",compute_cap"

	// The published cuda12 llama-server build is compiled for compute
	// capability 7.5 and newer only, with no fallback kernels for older
	// architectures: Pascal (6.1, GTX 10-series) and Volta (7.0) cards
	// start it and then fail at model load. Keep in step with the CUDA
	// architecture list of the runtime build.
	minCUDACapMajor = 7
	minCUDACapMinor = 5
)

// nvidiaGPU is one nvidia-smi row. The compute capability stays here and
// never reaches the profile: GpuInfo has no field for it, and its only
// consumer is the lane decision in nvidiaGPUInfos.
type nvidiaGPU struct {
	Name          string
	VRAMMB        uint64
	DriverVersion string
	// CapMajor.CapMinor is the CUDA compute capability; CapKnown is false
	// when the driver was not asked, did not answer, or answered something
	// that is not a plain "major.minor".
	CapMajor, CapMinor int
	CapKnown           bool
}

// runsCUDARuntime reports whether the cuda12 build can run on this GPU.
// Unknown capability counts as yes: only a cleanly parsed value below the
// floor takes a card off the CUDA lane.
func (g nvidiaGPU) runsCUDARuntime() bool {
	if !g.CapKnown {
		return true
	}
	return g.CapMajor > minCUDACapMajor ||
		(g.CapMajor == minCUDACapMajor && g.CapMinor >= minCUDACapMinor)
}

// parseNvidiaSMI reads one `name, memory.total, driver_version` line per
// GPU, with an optional fourth `compute_cap` column. Lines with fewer than
// three fields (error text, blank lines) are skipped.
func parseNvidiaSMI(out string) []nvidiaGPU {
	var gpus []nvidiaGPU
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		parts := strings.Split(line, ",")
		if len(parts) < 3 {
			continue
		}
		memMB, _ := strconv.ParseUint(strings.TrimSpace(parts[1]), 10, 64)
		g := nvidiaGPU{
			Name:          strings.TrimSpace(parts[0]),
			VRAMMB:        memMB,
			DriverVersion: strings.TrimSpace(parts[2]),
		}
		// Exactly four fields: with any other count the columns cannot be
		// trusted to line up, and a misread capability must not demote.
		if len(parts) == 4 {
			g.CapMajor, g.CapMinor, g.CapKnown = parseComputeCap(parts[3])
		}
		gpus = append(gpus, g)
	}
	return gpus
}

// parseComputeCap parses nvidia-smi's compute_cap value: "major.minor" in
// plain ASCII digits ("6.1", "12.0"). Anything else ("[N/A]", "", a bare
// "7", a sign, a third component, major 0) is not ok.
func parseComputeCap(s string) (major, minor int, ok bool) {
	majS, minS, found := strings.Cut(strings.TrimSpace(s), ".")
	if !found || !isShortDecimal(majS) || !isShortDecimal(minS) {
		return 0, 0, false
	}
	major, _ = strconv.Atoi(majS)
	minor, _ = strconv.Atoi(minS)
	if major == 0 {
		return 0, 0, false // no such architecture: treat as unreadable
	}
	return major, minor, true
}

// isShortDecimal reports whether s is one to three ASCII digits.
func isShortDecimal(s string) bool {
	if len(s) == 0 || len(s) > 3 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// queryNvidiaSMI asks for the GPU list through run, which executes
// nvidia-smi with the given --query-gpu field list. The compute_cap query
// goes first; when it fails or yields no GPU row (a driver that predates
// the field rejects the query outright) the base query is used and every
// capability is unknown. Nil when the base query fails too.
func queryNvidiaSMI(run func(fields string) ([]byte, error)) []nvidiaGPU {
	if out, err := run(nvidiaSMIFieldsWithCap); err == nil {
		if gpus := parseNvidiaSMI(string(out)); len(gpus) > 0 {
			return gpus
		}
	}
	out, err := run(nvidiaSMIFields)
	if err != nil {
		return nil
	}
	return parseNvidiaSMI(string(out))
}

// nvidiaGPUInfos turns nvidia-smi rows into profile entries and decides
// their accelerator lane, returning one warning per GPU the cuda12 build
// cannot run.
//
// The decision is per box, not per card, because a node runs one runtime
// build (see pickLinuxGPUs) and the profile's first GPU names its lane:
//
//   - at least one GPU can run the CUDA build (or its capability is
//     unknown): every GPU keeps Accel cuda12, exactly as before; a card
//     below the floor is only warned about, so it cannot pull a newer card
//     off CUDA whatever order nvidia-smi lists them in.
//   - every GPU is known to be below the floor: they are reported with no
//     native lane (empty Accel, VRAM kept), which accelChain already
//     resolves to vulkan when the NVIDIA Vulkan driver is present and to
//     the CPU build otherwise.
//
// vulkan and goos only shape the warning text: whether the Vulkan lane is
// available for NVIDIA here, and whether installing it is an option.
func nvidiaGPUInfos(rows []nvidiaGPU, vulkan bool, goos string) (gpus []*typesv1.GpuInfo, warnings []string) {
	anyCUDA := false
	for _, r := range rows {
		if r.runsCUDARuntime() {
			anyCUDA = true
			break
		}
	}
	for _, r := range rows {
		accel := accelCUDA12
		if !r.runsCUDARuntime() {
			if !anyCUDA {
				accel = ""
			}
			warnings = append(warnings, nvidiaCapWarning(r, anyCUDA, vulkan, goos))
		}
		gpus = append(gpus, &typesv1.GpuInfo{
			Vendor:        "nvidia",
			Model:         r.Name,
			VramMb:        r.VRAMMB,
			DriverVersion: r.DriverVersion,
			Accel:         accel,
		})
	}
	return gpus, warnings
}

// nvidiaCapWarning is the operator-facing line for a GPU below the CUDA
// floor: the card, its capability, what the CUDA runtime needs, and what
// the node does instead.
func nvidiaCapWarning(g nvidiaGPU, mixed, vulkan bool, goos string) string {
	msg := fmt.Sprintf("nvidia gpu too old for the CUDA runtime: %s has compute capability %d.%d, "+
		"the CUDA runtime needs %d.%d or newer (RTX 20-series / GTX 16-series or newer)",
		g.Name, g.CapMajor, g.CapMinor, minCUDACapMajor, minCUDACapMinor)
	switch {
	case mixed:
		return msg + "; the node stays on the CUDA runtime for its other NVIDIA GPU, and this card cannot serve on it"
	case vulkan:
		return msg + "; falling back to the Vulkan runtime"
	case goos == "linux":
		return msg + "; falling back to the CPU runtime (install the Vulkan loader and the NVIDIA Vulkan driver to serve on this GPU through the Vulkan runtime)"
	default:
		return msg + "; falling back to the CPU runtime"
	}
}

// NVIDIAGPU is one NVIDIA card as its driver reports it, for diagnostics
// (`tera doctor`): what the capability profile cannot carry, and the
// operator-facing explanation when the published CUDA build cannot run on
// the card.
type NVIDIAGPU struct {
	Name          string
	DriverVersion string
	VRAMMB        uint64
	// ComputeCap is the CUDA compute capability ("8.9"); empty when the
	// driver does not report one.
	ComputeCap string
	// RunsCUDA is false only when the capability is known to be below what
	// the CUDA build needs.
	RunsCUDA bool
	// Warning is the same line detection logs for a card below the floor;
	// empty otherwise.
	Warning string
}

// nvidiaReport is the pure half of ProbeNVIDIA: nvidia-smi rows to
// diagnostics, with the warning text nvidiaGPUInfos would log.
func nvidiaReport(rows []nvidiaGPU, vulkan bool, goos string) []NVIDIAGPU {
	anyCUDA := false
	for _, r := range rows {
		if r.runsCUDARuntime() {
			anyCUDA = true
			break
		}
	}
	var out []NVIDIAGPU
	for _, r := range rows {
		g := NVIDIAGPU{Name: r.Name, DriverVersion: r.DriverVersion, VRAMMB: r.VRAMMB, RunsCUDA: r.runsCUDARuntime()}
		if r.CapKnown {
			g.ComputeCap = fmt.Sprintf("%d.%d", r.CapMajor, r.CapMinor)
		}
		if !g.RunsCUDA {
			g.Warning = nvidiaCapWarning(r, anyCUDA, vulkan, goos)
		}
		out = append(out, g)
	}
	return out
}
