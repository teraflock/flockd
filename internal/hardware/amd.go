package hardware

// AMD GPU detection for Linux (teraflock/flockd#20).
//
// sysfs is the primary source: the amdgpu kernel driver exposes vendor,
// device id and VRAM size under /sys/class/drm/card*/device on every
// distro, ROCm userland or not. rocm-smi, when installed, only enriches
// what sysfs found (marketing name, driver version). Everything here takes
// a root directory so the walker and parsers run against testdata trees on
// any OS; hw_linux.go wires them to "/" and the real rocm-smi.
//
// No cgo, no ROCm library dependency (SPEC §A1.3: subprocess only, the
// release is cross-compiled).

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
)

const (
	amdPCIVendorID = "0x1002"

	// minAMDVRAMMB is the smallest VRAM an AMD GPU needs to be reported.
	// Integrated APUs reserve a BIOS carveout (typically 256 MB–2 GB) that
	// no catalog model fits, and reporting one would place the node as a
	// GPU box it cannot serve as. Skipped GPUs are logged at debug, never
	// dropped silently. Large-carveout APUs (Strix Halo class) clear the
	// bar; using GTT beyond the carveout is a llama.cpp/ROCm concern, not
	// detection's.
	minAMDVRAMMB = 2048
)

// drmCardRe matches primary DRM nodes (card0, card1) and excludes
// connectors (card1-DP-1) and render nodes (renderD128).
var drmCardRe = regexp.MustCompile(`^card\d+$`)

// amdSysfsGPU is one AMD GPU as read from sysfs.
type amdSysfsGPU struct {
	Card        string // drm node name, e.g. card1
	PCISlot     string // PCI_SLOT_NAME from uevent, e.g. 0000:03:00.0
	DeviceID    string // PCI device id, lowercase with 0x prefix, e.g. 0x744c
	SubsysID    string // PCI_SUBSYS_ID from uevent, lowercase "vvvv:dddd"
	ProductName string // sysfs product_name when the VBIOS/FRU exposes it
	VRAMMB      uint64 // mem_info_vram_total in MiB
}

// amdSysfsGPUs walks <root>/sys/class/drm/card*/device for AMD devices
// bound to amdgpu. It returns every AMD GPU, including ones below
// minAMDVRAMMB; the threshold is applied by buildAMDGPUs so the walker
// stays a faithful view of the tree.
func amdSysfsGPUs(root string) []amdSysfsGPU {
	cards, _ := filepath.Glob(filepath.Join(root, "sys", "class", "drm", "card*"))
	sort.Strings(cards)
	var gpus []amdSysfsGPU
	for _, card := range cards {
		if !drmCardRe.MatchString(filepath.Base(card)) {
			continue
		}
		dev := filepath.Join(card, "device")
		if sysfsValue(dev, "vendor") != amdPCIVendorID {
			continue
		}
		g := amdSysfsGPU{
			Card:        filepath.Base(card),
			DeviceID:    strings.ToLower(sysfsValue(dev, "device")),
			ProductName: sysfsValue(dev, "product_name"),
		}
		for k, v := range parseUevent(filepath.Join(dev, "uevent")) {
			switch k {
			case "PCI_SLOT_NAME":
				g.PCISlot = strings.ToLower(v)
			case "PCI_SUBSYS_ID":
				g.SubsysID = strings.ToLower(v)
			}
		}
		if b, err := strconv.ParseUint(sysfsValue(dev, "mem_info_vram_total"), 10, 64); err == nil {
			g.VRAMMB = b / (1024 * 1024)
		}
		gpus = append(gpus, g)
	}
	return gpus
}

// amdUnboundPCI lists PCI slots of AMD display-class devices
// (class 0x03xxxx under <root>/sys/bus/pci/devices) that have no DRM node,
// i.e. no amdgpu driver bound (blacklisted module, wrong kernel, VFIO
// passthrough). Such a GPU cannot run ROCm or Vulkan, so it is reported in
// the log rather than the profile.
func amdUnboundPCI(root string, bound []amdSysfsGPU) []string {
	seen := map[string]bool{}
	for _, g := range bound {
		seen[g.PCISlot] = true
	}
	devs, _ := filepath.Glob(filepath.Join(root, "sys", "bus", "pci", "devices", "*"))
	sort.Strings(devs)
	var unbound []string
	for _, d := range devs {
		if sysfsValue(d, "vendor") != amdPCIVendorID || !strings.HasPrefix(sysfsValue(d, "class"), "0x03") {
			continue
		}
		if slot := strings.ToLower(filepath.Base(d)); !seen[slot] {
			unbound = append(unbound, slot)
		}
	}
	return unbound
}

// amdModuleVersion reads <root>/sys/module/amdgpu/version, which only the
// out-of-tree (DKMS / ROCm-packaged) driver provides; the in-tree module
// has no version file and the kernel release stands in for it.
func amdModuleVersion(root string) string {
	return sysfsValue(filepath.Join(root, "sys", "module", "amdgpu"), "version")
}

func sysfsValue(dir, name string) string {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func parseUevent(path string) map[string]string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	kv := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok {
			kv[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return kv
}

// pciIDsPaths are where distros install the PCI ID database (hwdata on
// Fedora/Arch, misc on Debian/Ubuntu). Read relative to root.
var pciIDsPaths = []string{
	"usr/share/hwdata/pci.ids",
	"usr/share/misc/pci.ids",
	"usr/share/pci.ids",
}

// pciIDsName resolves an AMD device id (0x744c) and optional subsystem id
// ("1002:0e3b") to names from pci.ids: (subsystem name, device name). The
// subsystem entry is the retail SKU ("Radeon RX 7900 XTX"); the device
// entry is the silicon family ("Navi 31 [Radeon RX 7900 XT/7900 XTX/7900M]").
// Empty strings when the database or the ids are absent.
func pciIDsName(root, deviceID, subsysID string) (subsys, device string) {
	var f *os.File
	for _, p := range pciIDsPaths {
		var err error
		if f, err = os.Open(filepath.Join(root, p)); err == nil {
			break
		}
		f = nil
	}
	if f == nil {
		return "", ""
	}
	defer f.Close()

	devHex := strings.TrimPrefix(strings.ToLower(deviceID), "0x")
	subVendor, subDevice, _ := strings.Cut(strings.ToLower(subsysID), ":")
	inVendor, inDevice := false, false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		switch {
		case strings.HasPrefix(line, "\t\t"):
			if !inDevice {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) >= 3 && fields[0] == subVendor && fields[1] == subDevice {
				subsys = strings.Join(fields[2:], " ")
				return subsys, device
			}
		case strings.HasPrefix(line, "\t"):
			if !inVendor {
				continue
			}
			if inDevice {
				// Left the device block without a subsystem match.
				return "", device
			}
			id, name, _ := strings.Cut(strings.TrimSpace(line), "  ")
			if id == devHex {
				inDevice = true
				device = strings.TrimSpace(name)
			}
		default:
			if inVendor {
				return "", device // vendor block ended
			}
			id, _, _ := strings.Cut(line, "  ")
			if id == "1002" {
				inVendor = true
			}
		}
	}
	return "", device
}

// rocmSMICard is the subset of one rocm-smi card record the daemon uses.
type rocmSMICard struct {
	Bus      string // PCI bus, e.g. 0000:03:00.0 (empty on old releases)
	Series   string // "Card series" / "Device Name"
	DeviceID string // "Card model" / "Device ID", e.g. 0x744c
	VRAMMB   uint64
}

// rocmSMIInfo is the parsed output of rocmSMIArgs.
type rocmSMIInfo struct {
	Cards         []rocmSMICard
	DriverVersion string
}

// parseRocmSMI parses `rocm-smi ... --json` (rocmSMIArgs). The tool prints one JSON
// object keyed "card0".."cardN" plus "system"; field names changed case
// between ROCm 5 ("Card series") and ROCm 6 ("Card Series"), so lookups
// are case-insensitive. Leading non-JSON output (warnings) is skipped.
func parseRocmSMI(raw []byte) (rocmSMIInfo, error) {
	var info rocmSMIInfo
	start := strings.IndexByte(string(raw), '{')
	if start < 0 {
		return info, fmt.Errorf("rocm-smi: no JSON object in output")
	}
	var top map[string]map[string]any
	if err := json.Unmarshal(raw[start:], &top); err != nil {
		return info, fmt.Errorf("rocm-smi: parse json: %w", err)
	}
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	// card2 sorts after card10 lexically; order by the numeric suffix.
	sort.Slice(keys, func(i, j int) bool { return rocmCardIndex(keys[i]) < rocmCardIndex(keys[j]) })
	for _, k := range keys {
		fields := lowerKeys(top[k])
		if k == "system" {
			info.DriverVersion = fields["driver version"]
			continue
		}
		if !strings.HasPrefix(k, "card") {
			continue
		}
		c := rocmSMICard{
			Bus:      strings.ToLower(fields["pci bus"]),
			Series:   firstNonEmpty(fields["card series"], fields["device name"]),
			DeviceID: strings.ToLower(firstNonEmpty(fields["card model"], fields["device id"])),
		}
		if b, err := strconv.ParseUint(fields["vram total memory (b)"], 10, 64); err == nil {
			c.VRAMMB = b / (1024 * 1024)
		}
		info.Cards = append(info.Cards, c)
	}
	return info, nil
}

func rocmCardIndex(key string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(key, "card"))
	if err != nil {
		return -1
	}
	return n
}

func lowerKeys(m map[string]any) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		switch t := v.(type) {
		case string:
			out[strings.ToLower(k)] = strings.TrimSpace(t)
		case float64:
			out[strings.ToLower(k)] = strconv.FormatFloat(t, 'f', -1, 64)
		}
	}
	return out
}

// buildAMDGPUs merges the sysfs view with optional rocm-smi enrichment
// into GpuInfo records. kernel is the running kernel release, the driver
// version of last resort for the in-tree module.
//
// Model name precedence: sysfs product_name (VBIOS/FRU string, mostly
// server cards) > pci.ids subsystem (retail SKU) > rocm-smi series >
// pci.ids device (silicon family) > "AMD GPU <device id>".
func buildAMDGPUs(root string, sys []amdSysfsGPU, smi rocmSMIInfo, kernel string) []*typesv1.GpuInfo {
	log := slog.Default()
	driver := firstNonEmpty(amdModuleVersion(root), smi.DriverVersion)
	if driver == "" && kernel != "" {
		driver = "amdgpu (kernel " + kernel + ")"
	}
	var gpus []*typesv1.GpuInfo
	for i, g := range sys {
		card := matchRocmCard(smi.Cards, g, i, len(sys))
		subsys, device := pciIDsName(root, g.DeviceID, g.SubsysID)
		model := firstNonEmpty(g.ProductName, subsys, card.Series, device)
		if model == "" {
			model = "AMD GPU " + g.DeviceID
		}
		vram := g.VRAMMB
		if vram == 0 {
			vram = card.VRAMMB
		}
		if vram < minAMDVRAMMB {
			log.Debug("amd gpu below VRAM threshold; not reported (APU carveout?)",
				"card", g.Card, "model", model, "vram_mb", vram, "min_mb", minAMDVRAMMB)
			continue
		}
		gpus = append(gpus, &typesv1.GpuInfo{
			Vendor:        "amd",
			Model:         model,
			VramMb:        vram,
			DriverVersion: driver,
			Accel:         accelROCm,
		})
	}
	return gpus
}

// matchRocmCard pairs a sysfs GPU with its rocm-smi record by PCI bus, or
// by position when rocm-smi predates --showbus and enumerated the same
// number of cards. Zero value when nothing pairs.
func matchRocmCard(cards []rocmSMICard, g amdSysfsGPU, i, n int) rocmSMICard {
	for _, c := range cards {
		if c.Bus != "" && c.Bus == g.PCISlot {
			return c
		}
	}
	if len(cards) == n && i < len(cards) && cards[i].Bus == "" {
		return cards[i]
	}
	return rocmSMICard{}
}

// pickLinuxGPUs keeps the NVIDIA list when nvidia-smi found anything and
// only then consults AMD detection: a mixed box serves on CUDA (one runtime
// per node), and the ordering is what flockd#20 asked to preserve. amd is a
// thunk so the sysfs walk and rocm-smi call are skipped on NVIDIA boxes.
func pickLinuxGPUs(nvidia []*typesv1.GpuInfo, amd func() []*typesv1.GpuInfo) []*typesv1.GpuInfo {
	if len(nvidia) > 0 {
		return nvidia
	}
	return amd()
}
