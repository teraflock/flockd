package hardware

import (
	"slices"

	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
)

// Accelerator preference (teraflock/flockd#21).
//
// The wire profile carries one Accel per GPU — the vendor's native backend
// (cuda12, rocm, metal). Which llama-server build a node actually runs is
// decided here, daemon-locally, as an ordered chain the runtime fetcher
// walks until it finds a published artifact:
//
//	cuda12 > rocm > vulkan > cpu-avx2 / cpu
//
// CUDA and ROCm are fastest (tok/s is payout); Vulkan is the portable GPU
// lane — AMD boxes without ROCm, Intel Arc, NVIDIA boxes without the CUDA
// toolkit — and sits above CPU. Vulkan is never the profile's Accel and
// never its own GpuInfo: it is a secondary lane of a detected GPU, present
// only when the Vulkan loader and a hardware ICD for that GPU's vendor are
// installed (vulkan.go). macOS never emits it (Metal always wins; MoltenVK
// is not a target).

// AccelPreference returns the ordered accelerator chain the runtime
// artifact fetcher should try for this profile: the native backend of each
// detected GPU, then vulkan when it is available for that GPU's vendor,
// then the CPU build. Always non-empty; BestAccel is its head.
func AccelPreference(p *typesv1.CapabilityProfile) []string {
	return accelChain(p, vulkanVendors())
}

// accelChain is AccelPreference with the Vulkan probe result injected:
// vulkan[vendor] is true when a loader plus that vendor's hardware ICD were
// found. Split out so the chain is unit-testable on every OS.
func accelChain(p *typesv1.CapabilityProfile, vulkan map[string]bool) []string {
	var chain []string
	add := func(a string) {
		if a != "" && !slices.Contains(chain, a) {
			chain = append(chain, a)
		}
	}
	for _, g := range p.GetGpus() {
		v := g.GetVendor()
		if v == "" || v == "none" {
			continue
		}
		switch a := g.GetAccel(); a {
		case "", accelCPU, accelCPUAVX2:
			// A GPU with no native lane (Intel Arc, an adapter Windows lists
			// without a backend): Vulkan is its only GPU option.
		default:
			add(a)
		}
		if vulkan[v] {
			add(accelVulkan)
		}
	}
	add(cpuAccel())
	return chain
}
