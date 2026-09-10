package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AMD discrete GPUs (teraflock/flockd#16). Two sources, tried in order:
//
//  1. sysfs: the amdgpu kernel driver exposes mem_info_vram_used (bytes)
//     under /sys/class/drm/card*/device on every distro, ROCm userland or
//     not. One file read per card per tick, no subprocess — the same
//     tree hardware/amd.go detects the cards from.
//  2. rocm-smi --showmeminfo vram --json, whose per-card "VRAM Total Used
//     Memory (B)" is the byte figure (--showmemuse only reports
//     percentages). Used when sysfs shows no amdgpu card, which in
//     practice means a container without /sys or an exotic driver stack.
//
// Neither exists on Windows (AMD ships no rocm-smi there), so AMD cards
// on Windows keep the estimate. Both parsers take their input explicitly
// so the fixture tests run on any OS.

// errNoAMDSysfs means the sysfs tree has no amdgpu card to read.
var errNoAMDSysfs = errors.New("memory: no amdgpu card in sysfs")

// amdSysfsRoot is prepended to the sysfs path; tests point it at a
// fixture tree.
var amdSysfsRoot = "/"

// rocmSMITimeout bounds one rocm-smi call, like nvidiaSMITimeout.
var rocmSMITimeout = 3 * time.Second

// rocmSMIArgs is the query. --showmeminfo vram has printed the used
// figure since ROCm 3.x; --json since ROCm 4.
var rocmSMIArgs = []string{"--showmeminfo", "vram", "--json"}

// amdPCIVendorID is the vendor id sysfs reports for AMD/ATI devices.
const amdPCIVendorID = "0x1002"

// drmCardRe matches primary DRM nodes (card0, card1) and excludes
// connectors (card1-DP-1) and render nodes (renderD128).
var drmCardRe = regexp.MustCompile(`^card\d+$`)

// AMDVRAMUsedMB sums used VRAM across every AMD GPU: sysfs first, rocm-smi
// when sysfs has no amdgpu card. ErrNoGPUTool when neither is available.
func AMDVRAMUsedMB(ctx context.Context) (int64, error) {
	mb, err := amdSysfsVRAMUsedMB(amdSysfsRoot)
	if !errors.Is(err, errNoAMDSysfs) {
		return mb, err
	}
	return rocmSMIVRAMUsedMB(ctx)
}

// amdSysfsVRAMUsedMB sums <root>/sys/class/drm/card*/device/mem_info_vram_used
// over cards whose vendor is AMD. errNoAMDSysfs when there is none; a card
// that exists but cannot be read is an error (the caller keeps its previous
// figure rather than under-counting).
func amdSysfsVRAMUsedMB(root string) (int64, error) {
	cards, _ := filepath.Glob(filepath.Join(root, "sys", "class", "drm", "card*"))
	sort.Strings(cards)
	var total uint64
	found := 0
	for _, card := range cards {
		if !drmCardRe.MatchString(filepath.Base(card)) {
			continue
		}
		dev := filepath.Join(card, "device")
		if vendor, err := os.ReadFile(filepath.Join(dev, "vendor")); err != nil || strings.TrimSpace(string(vendor)) != amdPCIVendorID {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dev, "mem_info_vram_used"))
		if err != nil {
			return 0, fmt.Errorf("memory: %w", err)
		}
		b, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("memory: parse %s/mem_info_vram_used: %w", filepath.Base(card), err)
		}
		total += b
		found++
	}
	if found == 0 {
		return 0, errNoAMDSysfs
	}
	return int64(total / MiB), nil
}

// rocmSMIVRAMUsedMB runs rocm-smi and sums the per-card used figure.
func rocmSMIVRAMUsedMB(ctx context.Context) (int64, error) {
	path, err := exec.LookPath("rocm-smi")
	if err != nil {
		return 0, ErrNoGPUTool
	}
	ctx, cancel := context.WithTimeout(ctx, rocmSMITimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, rocmSMIArgs...).Output()
	if err != nil {
		return 0, fmt.Errorf("memory: rocm-smi: %w", err)
	}
	return parseRocmSMIVRAMUsed(out)
}

// parseRocmSMIVRAMUsed sums "VRAM Total Used Memory (B)" over the card
// records of `rocm-smi --showmeminfo vram --json`. The tool prints one
// object keyed "card0".."cardN" plus "system"; key case differs between
// ROCm 5 and 6, so lookups are case-insensitive, and leading non-JSON
// output (version warnings) is skipped.
func parseRocmSMIVRAMUsed(raw []byte) (int64, error) {
	start := strings.IndexByte(string(raw), '{')
	if start < 0 {
		return 0, errors.New("memory: rocm-smi: no JSON object in output")
	}
	var top map[string]map[string]any
	if err := json.Unmarshal(raw[start:], &top); err != nil {
		return 0, fmt.Errorf("memory: rocm-smi: parse json: %w", err)
	}
	var total uint64
	found := 0
	for key, fields := range top {
		if !strings.HasPrefix(key, "card") {
			continue
		}
		for k, v := range fields {
			if strings.ToLower(k) != "vram total used memory (b)" {
				continue
			}
			var s string
			switch t := v.(type) {
			case string:
				s = strings.TrimSpace(t)
			case float64:
				s = strconv.FormatFloat(t, 'f', -1, 64)
			}
			b, err := strconv.ParseUint(s, 10, 64)
			if err != nil {
				return 0, fmt.Errorf("memory: rocm-smi: parse %s used %q: %w", key, s, err)
			}
			total += b
			found++
		}
	}
	if found == 0 {
		return 0, errors.New("memory: rocm-smi reported no VRAM usage (expected --showmeminfo vram output)")
	}
	return int64(total / MiB), nil
}
