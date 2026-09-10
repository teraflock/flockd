//go:build windows

package hardware

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unsafe"

	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// Windows probes (flockd#29), all cgo-free:
//   - RAM: kernel32 GlobalMemoryStatusEx (not wrapped by x/sys; lazy DLL).
//   - CPU: the marketing name from the registry
//     (HKLM\HARDWARE\DESCRIPTION\System\CentralProcessor\0\ProcessorNameString);
//     PROCESSOR_IDENTIFIER ("Intel64 Family 6 Model 154 ...") is the fallback.
//   - NVIDIA GPUs + VRAM: nvidia-smi (shared with Linux, hw_nvidia.go),
//     which the driver installs into System32 — on PATH for every box,
//     with the absolute path as a fallback for stripped-down services.
//   - Every other adapter (Intel iGPU, AMD): Win32_VideoController via
//     PowerShell/CIM, so the admin console can see the card. AdapterRAM
//     is a 32-bit field (caps at 4 GB) and unreliable for discrete cards,
//     so those report VramMb 0 and no accel: they stay on the CPU lane
//     until a Windows vulkan lane exists. AMD VRAM is not sampled on
//     Windows either (no rocm-smi / sysfs there; flockd#16 covers Linux).
var (
	kernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
)

// memoryStatusEx mirrors MEMORYSTATUSEX (sysinfoapi.h). dwLength must be
// set to the struct size before the call.
type memoryStatusEx struct {
	dwLength                uint32
	dwMemoryLoad            uint32
	ullTotalPhys            uint64
	ullAvailPhys            uint64
	ullTotalPageFile        uint64
	ullAvailPageFile        uint64
	ullTotalVirtual         uint64
	ullAvailVirtual         uint64
	ullAvailExtendedVirtual uint64
}

func detectPlatform(ctx context.Context, p *typesv1.CapabilityProfile) error {
	if total, err := totalPhysicalRAM(); err == nil {
		p.RamTotalMb = total / (1024 * 1024)
	}
	p.CpuModel = cpuModel()

	p.Gpus = detectNvidia(ctx, "nvidia-smi", nvidiaSMISystem32())
	// Non-NVIDIA adapters are listed, never accelerated; an NVIDIA card that
	// nvidia-smi already reported is not repeated from the CIM listing (and
	// one nvidia-smi could not see, e.g. a stale driver, is listed without
	// VRAM like any other adapter).
	adapters, err := detectVideoControllers(ctx)
	if err != nil {
		return nil
	}
	seenNvidia := len(p.Gpus) > 0
	for _, a := range adapters {
		if a.Vendor == "nvidia" && seenNvidia {
			continue
		}
		p.Gpus = append(p.Gpus, a)
	}
	return nil
}

// totalPhysicalRAM returns the installed physical memory in bytes.
func totalPhysicalRAM() (uint64, error) {
	if err := procGlobalMemoryStatusEx.Find(); err != nil {
		return 0, err
	}
	var ms memoryStatusEx
	ms.dwLength = uint32(unsafe.Sizeof(ms))
	r, _, e := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&ms)))
	if r == 0 {
		return 0, fmt.Errorf("hardware: GlobalMemoryStatusEx: %w", e)
	}
	return ms.ullTotalPhys, nil
}

const cpuRegistryKey = `HARDWARE\DESCRIPTION\System\CentralProcessor\0`

// cpuModel prefers the registry's ProcessorNameString and falls back to
// the PROCESSOR_IDENTIFIER family string.
func cpuModel() string {
	name, _ := readCPUNameRegistry()
	return pickCPUModel(name, os.Getenv("PROCESSOR_IDENTIFIER"))
}

func readCPUNameRegistry() (string, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, cpuRegistryKey, registry.QUERY_VALUE)
	if err != nil {
		return "", fmt.Errorf("hardware: open %s: %w", cpuRegistryKey, err)
	}
	defer k.Close()
	name, _, err := k.GetStringValue("ProcessorNameString")
	if err != nil {
		return "", fmt.Errorf("hardware: ProcessorNameString: %w", err)
	}
	return name, nil
}

// pickCPUModel is the pure half of cpuModel: the registry name when
// present (whitespace-collapsed: the value carries padding on some
// boards), else the environment family string.
func pickCPUModel(registryName, envIdentifier string) string {
	if n := strings.Join(strings.Fields(registryName), " "); n != "" {
		return n
	}
	return strings.TrimSpace(envIdentifier)
}

// nvidiaSMISystem32 is where the NVIDIA driver installs nvidia-smi.exe.
func nvidiaSMISystem32() string {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	return filepath.Join(root, "System32", "nvidia-smi.exe")
}

// videoController mirrors the Win32_VideoController fields we consume, as
// emitted by ConvertTo-Json (a single adapter is a bare object, several
// are an array; AdapterRAM is null for some virtual adapters).
type videoController struct {
	Name          string `json:"Name"`
	AdapterRAM    *int64 `json:"AdapterRAM"`
	DriverVersion string `json:"DriverVersion"`
	PNPDeviceID   string `json:"PNPDeviceID"`
}

const videoControllerScript = `Get-CimInstance Win32_VideoController | Select-Object Name,AdapterRAM,DriverVersion,PNPDeviceID | ConvertTo-Json -Compress`

// detectVideoControllers lists every display adapter through CIM. Windows
// PowerShell 5.1 (powershell.exe) ships with every supported Windows, so
// no COM binding is needed.
func detectVideoControllers(ctx context.Context) ([]*typesv1.GpuInfo, error) {
	out, err := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", videoControllerScript).Output()
	if err != nil {
		return nil, fmt.Errorf("hardware: Win32_VideoController: %w", err)
	}
	return parseVideoControllers(out)
}

// parseVideoControllers decodes the ConvertTo-Json output. Software and
// remote-display adapters (Microsoft Basic Display, RDP, Hyper-V) are
// dropped: they are not GPUs.
func parseVideoControllers(raw []byte) ([]*typesv1.GpuInfo, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, nil
	}
	var list []videoController
	if raw[0] == '[' {
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, fmt.Errorf("hardware: parse Win32_VideoController: %w", err)
		}
	} else {
		var one videoController
		if err := json.Unmarshal(raw, &one); err != nil {
			return nil, fmt.Errorf("hardware: parse Win32_VideoController: %w", err)
		}
		list = []videoController{one}
	}
	var gpus []*typesv1.GpuInfo
	for _, vc := range list {
		vendor := adapterVendor(vc.Name, vc.PNPDeviceID)
		if vendor == "" {
			continue
		}
		gpus = append(gpus, &typesv1.GpuInfo{
			Vendor:        vendor,
			Model:         strings.TrimSpace(vc.Name),
			DriverVersion: strings.TrimSpace(vc.DriverVersion),
			// VramMb stays 0 and Accel empty: see the file comment.
		})
	}
	return gpus, nil
}

// adapterVendor maps a PnP id (PCI\VEN_xxxx) or, failing that, the adapter
// name to the vendor strings the profile uses. "" means not a GPU.
func adapterVendor(name, pnpDeviceID string) string {
	id := strings.ToUpper(pnpDeviceID)
	n := strings.ToLower(name)
	switch {
	case strings.Contains(id, "VEN_10DE") || strings.Contains(n, "nvidia"):
		return "nvidia"
	case strings.Contains(id, "VEN_1002") || strings.Contains(id, "VEN_1022") ||
		strings.Contains(n, "amd") || strings.Contains(n, "radeon"):
		return "amd"
	case strings.Contains(id, "VEN_8086") || strings.Contains(n, "intel"):
		return "intel"
	}
	return ""
}

func diskFreeBytes(path string) (uint64, error) {
	var free, total, avail uint64
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
		return 0, err
	}
	return avail, nil
}
