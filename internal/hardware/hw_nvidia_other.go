//go:build !linux && !windows

package hardware

import "context"

// ProbeNVIDIA reports nothing here: NVIDIA GPUs are only detected on Linux
// and Windows (hw_nvidia.go).
func ProbeNVIDIA(context.Context) []NVIDIAGPU { return nil }
