//go:build linux || windows

package hardware

import (
	"context"
	"testing"
)

func TestParseNvidiaSMI(t *testing.T) {
	gpus := parseNvidiaSMI("NVIDIA GeForce RTX 4090, 24564, 560.94\nNVIDIA RTX A2000, 6144, 560.94\n\n")
	if len(gpus) != 2 {
		t.Fatalf("gpus = %d, want 2", len(gpus))
	}
	g := gpus[0]
	if g.Vendor != "nvidia" || g.Model != "NVIDIA GeForce RTX 4090" || g.VramMb != 24564 ||
		g.DriverVersion != "560.94" || g.Accel != "cuda12" {
		t.Errorf("gpu[0] = %+v", g)
	}
	if gpus[1].VramMb != 6144 {
		t.Errorf("gpu[1].VramMb = %d", gpus[1].VramMb)
	}
	if got := parseNvidiaSMI(""); len(got) != 0 {
		t.Errorf("empty output -> %d gpus", len(got))
	}
	if got := parseNvidiaSMI("No devices were found\n"); len(got) != 0 {
		t.Errorf("error text -> %d gpus", len(got))
	}
}

// A candidate that does not exist is skipped, not an error, and a bare
// name that is not on PATH yields nil — the CPU-only fallback path.
func TestDetectNvidiaMissingTool(t *testing.T) {
	got := detectNvidia(context.Background(), "definitely-not-nvidia-smi-xyz", `/nonexistent/dir/nvidia-smi`)
	if got != nil {
		t.Errorf("detectNvidia with no tool = %v, want nil", got)
	}
}
