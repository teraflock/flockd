// Package hardware detects the node's compute capability and produces the
// typesv1.CapabilityProfile sent at enrollment and in heartbeats.
//
// Platform-specific probes live in build-tagged files (hw_darwin.go,
// hw_linux.go, hw_windows.go); this file holds the shared assembly.
package hardware

import (
	"context"
	"fmt"
	"runtime"

	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
)

// Version is stamped by the linker (goreleaser) and reported in the profile.
var Version = "dev"

// Detect probes the local machine. Failures of individual probes degrade
// gracefully (e.g. no GPU -> CPU-only profile) rather than failing detection.
func Detect(ctx context.Context, dataDir string) (*typesv1.CapabilityProfile, error) {
	p := &typesv1.CapabilityProfile{
		Os:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		CpuCores:      uint32(runtime.NumCPU()),
		DaemonVersion: Version,
	}

	if err := detectPlatform(ctx, p); err != nil {
		return nil, fmt.Errorf("hardware: platform detection: %w", err)
	}

	if free, err := diskFreeMB(dataDir); err == nil {
		p.DiskAvailableMb = free
	}

	if len(p.Gpus) == 0 {
		// CPU-only fallback: still serveable via llama.cpp cpu builds.
		p.Gpus = append(p.Gpus, &typesv1.GpuInfo{
			Vendor: "none",
			Model:  "cpu",
			Accel:  cpuAccel(),
		})
	}
	return p, nil
}

func cpuAccel() string {
	if runtime.GOARCH == "amd64" {
		return "cpu-avx2"
	}
	return "cpu"
}

// BestAccel returns the accelerator backend the runtime artifact fetcher
// should prefer for this profile.
func BestAccel(p *typesv1.CapabilityProfile) string {
	for _, g := range p.GetGpus() {
		if g.GetAccel() != "" && g.GetVendor() != "none" {
			return g.GetAccel()
		}
	}
	return cpuAccel()
}

// BestVRAMMB returns the largest detected GPU memory (system RAM on Apple
// Silicon's unified memory); 0 when no usable GPU was found.
func BestVRAMMB(p *typesv1.CapabilityProfile) uint64 {
	var best uint64
	for _, g := range p.GetGpus() {
		if g.GetVendor() != "none" && g.GetVramMb() > best {
			best = g.GetVramMb()
		}
	}
	return best
}
