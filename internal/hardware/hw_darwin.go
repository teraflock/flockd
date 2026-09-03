//go:build darwin

package hardware

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
	"golang.org/x/sys/unix"
)

func detectPlatform(ctx context.Context, p *typesv1.CapabilityProfile) error {
	if mem, err := unix.SysctlUint64("hw.memsize"); err == nil {
		p.RamTotalMb = mem / (1024 * 1024)
	}
	if brand, err := unix.Sysctl("machdep.cpu.brand_string"); err == nil {
		p.CpuModel = strings.TrimSpace(brand)
	}

	gpus, err := detectDarwinGPUs(ctx, p.RamTotalMb)
	if err != nil {
		// Non-fatal: fall through to CPU-only profile.
		return nil
	}
	p.Gpus = gpus
	return nil
}

// spDisplays mirrors the subset of `system_profiler SPDisplaysDataType -json`
// output we consume.
type spDisplays struct {
	SPDisplaysDataType []struct {
		Name         string `json:"_name"`
		Model        string `json:"sppci_model"`
		Vendor       string `json:"spdisplays_vendor"`
		VRAM         string `json:"spdisplays_vram"`
		VRAMShared   string `json:"spdisplays_vram_shared"`
		MetalSupport string `json:"spdisplays_mtlgpufamilysupport"`
		Cores        string `json:"sppci_cores"`
	} `json:"SPDisplaysDataType"`
}

func detectDarwinGPUs(ctx context.Context, ramMB uint64) ([]*typesv1.GpuInfo, error) {
	out, err := exec.CommandContext(ctx, "system_profiler", "SPDisplaysDataType", "-json").Output()
	if err != nil {
		return nil, fmt.Errorf("system_profiler: %w", err)
	}
	return parseSPDisplays(out, ramMB)
}

func parseSPDisplays(raw []byte, ramMB uint64) ([]*typesv1.GpuInfo, error) {
	var sp spDisplays
	if err := json.Unmarshal(raw, &sp); err != nil {
		return nil, fmt.Errorf("parse system_profiler json: %w", err)
	}
	var gpus []*typesv1.GpuInfo
	for _, d := range sp.SPDisplaysDataType {
		name := d.Model
		if name == "" {
			name = d.Name
		}
		g := &typesv1.GpuInfo{
			Model:  name,
			Vendor: normalizeVendor(d.Vendor, name),
		}
		if g.Vendor == "apple" {
			// Apple Silicon: unified memory, VRAM budget = system RAM.
			g.UnifiedMemory = true
			g.VramMb = ramMB
			g.Accel = "metal"
		} else {
			g.VramMb = parseVRAMMB(firstNonEmpty(d.VRAM, d.VRAMShared))
			if d.MetalSupport != "" {
				g.Accel = "metal"
			} else {
				g.Accel = "cpu"
			}
		}
		gpus = append(gpus, g)
	}
	return gpus, nil
}

func normalizeVendor(vendor, model string) string {
	v := strings.ToLower(vendor + " " + model)
	switch {
	case strings.Contains(v, "apple"):
		return "apple"
	case strings.Contains(v, "nvidia"):
		return "nvidia"
	case strings.Contains(v, "amd"), strings.Contains(v, "radeon"):
		return "amd"
	case strings.Contains(v, "intel"):
		return "intel"
	default:
		return strings.ToLower(strings.TrimSpace(vendor))
	}
}

var vramRe = regexp.MustCompile(`(\d+)\s*(MB|GB)`)

func parseVRAMMB(s string) uint64 {
	m := vramRe.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	n, err := strconv.ParseUint(m[1], 10, 64)
	if err != nil {
		return 0
	}
	if m[2] == "GB" {
		n *= 1024
	}
	return n
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
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
