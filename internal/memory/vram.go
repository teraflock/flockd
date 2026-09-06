package memory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
)

// Discrete GPUs keep the weights in VRAM, which the host-side footprint
// (proc_pid_rusage / smaps_rollup) cannot see. On NVIDIA cards nvidia-smi
// reports the card's used memory, so the daemon samples it — at most once
// per housekeeping tick, with a short timeout — and charges admission and
// the heartbeat's vram_used_mb with the measurement instead of the
// pre-load estimate. The figure is card-wide (compositor, other apps,
// every llama-server on the box), which is the conservative side for
// admission. AMD (rocm-smi) is a TODO; Windows host footprints stay
// estimates, but nvidia-smi works there too when it is on PATH.

// ErrNoGPUTool means no VRAM query tool (nvidia-smi) is on PATH.
var ErrNoGPUTool = errors.New("memory: no VRAM query tool on PATH")

// nvidiaSMITimeout bounds one nvidia-smi call: a wedged driver must not
// stall housekeeping.
var nvidiaSMITimeout = 3 * time.Second

// nvidiaSMIArgs is the query; --format csv,noheader,nounits yields one
// integer (MiB) per GPU, one per line.
var nvidiaSMIArgs = []string{"--query-gpu=memory.used", "--format=csv,noheader,nounits"}

// NvidiaVRAMUsedMB sums memory.used across every NVIDIA GPU nvidia-smi
// reports. ErrNoGPUTool when the tool is not installed.
func NvidiaVRAMUsedMB(ctx context.Context) (int64, error) {
	path, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return 0, ErrNoGPUTool
	}
	ctx, cancel := context.WithTimeout(ctx, nvidiaSMITimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, nvidiaSMIArgs...).Output()
	if err != nil {
		return 0, fmt.Errorf("memory: nvidia-smi: %w", err)
	}
	return parseNvidiaSMIMemoryUsed(string(out))
}

// parseNvidiaSMIMemoryUsed sums the per-GPU lines of
// `nvidia-smi --query-gpu=memory.used --format=csv,noheader,nounits`.
func parseNvidiaSMIMemoryUsed(out string) (int64, error) {
	var total int64
	lines := 0
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Tolerate a units suffix should a driver ignore nounits.
		line = strings.TrimSpace(strings.TrimSuffix(line, "MiB"))
		mb, err := strconv.ParseInt(line, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("memory: parse nvidia-smi memory.used %q: %w", line, err)
		}
		total += mb
		lines++
	}
	if lines == 0 {
		return 0, errors.New("memory: nvidia-smi reported no GPUs")
	}
	return total, nil
}

// VRAMSampler measures used VRAM on nodes where that is possible: a
// discrete NVIDIA GPU with nvidia-smi on PATH. Elsewhere Sample reports
// no measurement (once-logged) and callers keep their estimates.
type VRAMSampler struct {
	hw  *typesv1.CapabilityProfile
	log *slog.Logger
	// Query runs the tool (NvidiaVRAMUsedMB); tests substitute it.
	Query func(context.Context) (int64, error)

	mu       sync.Mutex
	disabled bool
	warned   bool
}

// NewVRAMSampler builds a sampler for the node's hardware.
func NewVRAMSampler(hw *typesv1.CapabilityProfile, log *slog.Logger) *VRAMSampler {
	return &VRAMSampler{hw: hw, log: log, Query: NvidiaVRAMUsedMB}
}

// Vendor is the discrete GPU vendor a sampler would measure ("nvidia",
// "amd", …), or "" when the node has no discrete GPU.
func (s *VRAMSampler) Vendor() string {
	if !Discrete(s.hw) {
		return ""
	}
	for _, g := range s.hw.GetGpus() {
		if v := strings.ToLower(g.GetVendor()); v != "none" && v != "" && g.GetVramMb() > 0 {
			return v
		}
	}
	return ""
}

// Sample measures used VRAM in MiB. ok is false when this node cannot be
// measured (no discrete GPU, unsupported vendor, tool missing) or when the
// query failed this time — the caller keeps its previous figure.
func (s *VRAMSampler) Sample(ctx context.Context) (mb int64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disabled {
		return 0, false
	}
	switch v := s.Vendor(); v {
	case "nvidia":
	case "":
		s.disabled = true
		return 0, false
	default:
		// TODO(rocm-smi): `rocm-smi --showmemuse --csv` for AMD. Until
		// then admission on AMD cards uses the pre-load estimate.
		s.disabled = true
		s.logger().Info("VRAM measurement not implemented for this GPU vendor; memory admission uses estimates", "vendor", v)
		return 0, false
	}
	mb, err := s.Query(ctx)
	if err != nil {
		if errors.Is(err, ErrNoGPUTool) {
			s.disabled = true
			s.logger().Info("nvidia-smi not on PATH; VRAM admission uses estimates")
			return 0, false
		}
		if !s.warned {
			s.warned = true
			s.logger().Warn("VRAM sample failed; keeping the estimate (further failures logged at debug)", "err", err)
		} else {
			s.logger().Debug("VRAM sample failed", "err", err)
		}
		return 0, false
	}
	return mb, true
}

func (s *VRAMSampler) logger() *slog.Logger {
	if s.log != nil {
		return s.log
	}
	return slog.Default()
}
