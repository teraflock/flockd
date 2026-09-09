package hardware

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Vulkan availability probe (teraflock/flockd#21). Two files decide it, no
// exec: the loader (libvulkan.so.1) and at least one installable client
// driver (ICD) manifest under the loader's search directories, whose
// library_path names the vendor driver. vulkaninfo is not required — it
// is often absent and needs a display. A software ICD (Mesa lavapipe)
// does not count: it would "work" at CPU speed while being paid as a GPU
// lane.
//
// The loader lookup is the only OS-specific piece: on Linux it is a stat
// per candidate lib dir; Windows (vulkan-1.dll + registry ICDs) is the
// Windows epic's job (teraflock/docs#9); macOS never probes.

// linuxVulkanLibDirs are where distros install the loader: Debian/Ubuntu
// multiarch, Fedora/Arch lib64/lib, /usr/local. LD_LIBRARY_PATH entries
// are appended at probe time. Distros with no fixed layout (NixOS) are not
// covered and fall to the next lane.
var linuxVulkanLibDirs = []string{
	"usr/lib/x86_64-linux-gnu",
	"usr/lib/aarch64-linux-gnu",
	"usr/lib64",
	"usr/lib",
	"lib64",
	"lib",
	"usr/local/lib64",
	"usr/local/lib",
}

// linuxVulkanICDDirs mirror the Vulkan loader's default ICD search order
// (sysconfdir, XDG_CONFIG_DIRS default, XDG_DATA_DIRS defaults).
var linuxVulkanICDDirs = []string{
	"usr/local/etc/vulkan/icd.d",
	"usr/local/share/vulkan/icd.d",
	"etc/vulkan/icd.d",
	"etc/xdg/vulkan/icd.d",
	"usr/share/vulkan/icd.d",
}

const vulkanLoaderSO = "libvulkan.so.1"

// vulkanProbe inspects a root (/ in production, a testdata tree in tests).
type vulkanProbe struct {
	root    string
	libDirs []string // relative to root, or absolute (LD_LIBRARY_PATH)
	icdDirs []string // relative to root
}

func newLinuxVulkanProbe(root string) vulkanProbe {
	p := vulkanProbe{root: root, libDirs: linuxVulkanLibDirs, icdDirs: linuxVulkanICDDirs}
	for _, d := range filepath.SplitList(os.Getenv("LD_LIBRARY_PATH")) {
		if d != "" {
			p.libDirs = append(p.libDirs, d)
		}
	}
	return p
}

// vendors reports which GPU vendors have a usable Vulkan driver: nil when
// the loader is missing (nothing can be usable), otherwise the set of
// vendors with a hardware ICD (possibly empty).
func (p vulkanProbe) vendors() map[string]bool {
	if !p.loaderPresent() {
		return nil
	}
	return p.icdVendors()
}

func (p vulkanProbe) loaderPresent() bool {
	for _, d := range p.libDirs {
		if !filepath.IsAbs(d) {
			d = filepath.Join(p.root, d)
		}
		if _, err := os.Stat(filepath.Join(d, vulkanLoaderSO)); err == nil {
			return true
		}
	}
	return false
}

// icdManifest is the subset of a Vulkan ICD manifest the probe reads.
type icdManifest struct {
	ICD struct {
		LibraryPath string `json:"library_path"`
	} `json:"ICD"`
}

func (p vulkanProbe) icdVendors() map[string]bool {
	vendors := map[string]bool{}
	for _, d := range p.icdDirs {
		files, _ := filepath.Glob(filepath.Join(p.root, d, "*.json"))
		sort.Strings(files)
		for _, f := range files {
			raw, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			var m icdManifest
			if err := json.Unmarshal(raw, &m); err != nil || m.ICD.LibraryPath == "" {
				continue
			}
			if v := icdVendor(m.ICD.LibraryPath); v != "" {
				vendors[v] = true
			}
		}
	}
	return vendors
}

// icdVendor maps an ICD's driver library to the GpuInfo vendor it serves;
// "" for software rasterisers and drivers for hardware flockd does not
// detect (virtio, broadcom, …).
func icdVendor(libraryPath string) string {
	lib := strings.ToLower(filepath.Base(libraryPath))
	switch {
	case strings.Contains(lib, "lvp"), strings.Contains(lib, "llvmpipe"), strings.Contains(lib, "swrast"):
		return "" // Mesa lavapipe: CPU
	case strings.Contains(lib, "radeon"), strings.Contains(lib, "amdvlk"), strings.Contains(lib, "amd"):
		return "amd" // RADV (libvulkan_radeon.so) or AMDVLK / AMD PRO (amdvlk64.so)
	case strings.Contains(lib, "nvidia"):
		return "nvidia" // libGLX_nvidia.so.0 / libnvidia-vulkan-producer
	case strings.Contains(lib, "intel"):
		return "intel" // ANV (libvulkan_intel.so, libvulkan_intel_hasvk.so)
	default:
		return ""
	}
}
