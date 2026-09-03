// Package memory is the daemon's model-memory accounting: the budget loaded
// models may use, the footprint estimate for a model before it is loaded,
// and the measured physical footprint of a running runtime process.
//
// Measurement is deliberately *not* RSS. llama.cpp mmaps GGUF weights, so
// RSS counts clean file-backed pages the kernel can drop at will and
// double counts the page cache; the numbers that matter are what macOS
// calls the physical footprint (proc_pid_rusage ri_phys_footprint, the
// figure Activity Monitor's "Memory" column and `footprint` show) and what
// Linux calls the proportional set size (/proc/<pid>/smaps_rollup Pss).
package memory

import (
	"errors"

	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
)

// ErrUnsupported means footprint measurement is not implemented on this
// platform; callers keep the pre-load estimate.
var ErrUnsupported = errors.New("memory: footprint measurement not supported on this platform")

// MiB is one mebibyte in bytes.
const MiB = 1024 * 1024

// Estimate parameters.
const (
	// weightsFactor covers weights plus the runtime's compute buffers and
	// allocator slack over the raw file size (measured 1.10–1.18x on
	// quantized GGUFs at moderate context on Metal).
	weightsFactor = 1.15
	// kvBytesPerTokenDivisor approximates KV-cache bytes per context token
	// from the weight file size: file_bytes / 65536. That reproduces the
	// f16 KV of GQA models within ~2x across the catalog's size range
	// (Llama-3 8B Q4: 75 KB/token estimated vs 128 KB real; Llama-3 70B
	// Q4: 610 KB vs 655 KB) without parsing GGUF metadata. TODO(gguf):
	// read block_count / n_head_kv / n_embd from the file header.
	kvBytesPerTokenDivisor = 65536
	// runtimeOverheadMB is the fixed cost of a llama-server process
	// (binary, Metal/CUDA context, tokenizer, HTTP server).
	runtimeOverheadMB = 256
	// DefaultContext is the context length assumed when neither the catalog
	// nor the operator sets one. llama-server's own default is 4096; the
	// larger figure errs on the side of not overfilling the machine.
	DefaultContext = 8192
	// unifiedFraction is the auto budget on unified-memory machines: the
	// GPU and the operator's own apps share one pool, and half of it is the
	// most a laptop can give to inference and still be a laptop.
	unifiedFraction = 0.5
)

// EstimateMB predicts the physical footprint of loading a model before it
// is loaded:
//
//	estimate = file_bytes × 1.15 + ctx × parallel × (file_bytes / 65536) + 256 MB
//
// When the catalog states min_ram_mb the larger of the two wins: the
// catalog value is authoritative for the weights but was written for a
// specific context, so a much larger operator-configured context still
// raises the estimate. ctx <= 0 uses DefaultContext; parallel < 1 is 1.
func EstimateMB(fileBytes int64, minRAMMB int64, ctx, parallel int) int64 {
	if ctx <= 0 {
		ctx = DefaultContext
	}
	if parallel < 1 {
		parallel = 1
	}
	weights := float64(fileBytes) * weightsFactor
	kv := float64(ctx) * float64(parallel) * (float64(fileBytes) / kvBytesPerTokenDivisor)
	est := int64((weights+kv)/MiB) + runtimeOverheadMB
	if minRAMMB > est {
		return minRAMMB
	}
	return est
}

// Unified reports whether the node's GPU shares system memory (Apple
// Silicon). On such machines the RAM budget is the whole story: there is no
// separate VRAM to fill.
func Unified(hw *typesv1.CapabilityProfile) bool {
	for _, g := range hw.GetGpus() {
		if g.GetUnifiedMemory() {
			return true
		}
	}
	return false
}

// Discrete reports whether the node has a discrete (non-unified) GPU, where
// weights live in VRAM that host-side footprint measurement cannot see.
func Discrete(hw *typesv1.CapabilityProfile) bool {
	if Unified(hw) {
		return false
	}
	for _, g := range hw.GetGpus() {
		if g.GetVendor() != "none" && g.GetVramMb() > 0 {
			return true
		}
	}
	return false
}

// AutoBudgetMB derives the model-memory budget when budget.max_ram_mb is 0:
// half of physical RAM on unified-memory (and CPU-only) machines, and
// vram × max_vram_percent on discrete GPUs, where the weights live on the
// card and host RAM is not the constraint.
func AutoBudgetMB(hw *typesv1.CapabilityProfile, maxVRAMPercent int) int64 {
	ram := int64(hw.GetRamTotalMb())
	if !Unified(hw) {
		var vram int64
		for _, g := range hw.GetGpus() {
			if g.GetVendor() != "none" && int64(g.GetVramMb()) > vram {
				vram = int64(g.GetVramMb())
			}
		}
		if vram > 0 {
			if maxVRAMPercent <= 0 || maxVRAMPercent > 100 {
				maxVRAMPercent = 80
			}
			return vram * int64(maxVRAMPercent) / 100
		}
	}
	return int64(float64(ram) * unifiedFraction)
}

// BudgetMB resolves the configured budget: configured > 0 wins, else auto.
func BudgetMB(configured int64, hw *typesv1.CapabilityProfile, maxVRAMPercent int) int64 {
	if configured > 0 {
		return configured
	}
	return AutoBudgetMB(hw, maxVRAMPercent)
}

// ProcessFootprintMB measures the physical footprint of a process in MiB.
// Returns ErrUnsupported where no implementation exists; callers then keep
// their estimate.
func ProcessFootprintMB(pid int) (int64, error) {
	b, err := processFootprintBytes(pid)
	if err != nil {
		return 0, err
	}
	return int64(b / MiB), nil
}
