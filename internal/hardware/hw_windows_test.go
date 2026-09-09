//go:build windows

package hardware

import (
	"context"
	"testing"

	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
)

// Live probes: these run for real on the windows-latest CI lane. The
// runner is a CPU-only VM (Hyper-V), so the assertions are about the
// calls succeeding, not about any particular card.
func TestWindowsRAMAndCPULive(t *testing.T) {
	total, err := totalPhysicalRAM()
	if err != nil {
		t.Fatalf("GlobalMemoryStatusEx: %v", err)
	}
	if total < 512*1024*1024 {
		t.Errorf("total physical RAM = %d bytes, implausibly small", total)
	}
	name, err := readCPUNameRegistry()
	if err != nil {
		t.Fatalf("registry ProcessorNameString: %v", err)
	}
	if name == "" {
		t.Error("ProcessorNameString empty")
	}
	if cpuModel() == "" {
		t.Error("cpuModel() empty")
	}
}

func TestWindowsVideoControllersLive(t *testing.T) {
	gpus, err := detectVideoControllers(context.Background())
	if err != nil {
		t.Fatalf("detectVideoControllers: %v", err)
	}
	// A Hyper-V runner lists only the Microsoft virtual adapter, which is
	// filtered out; the point is that PowerShell + CIM + the parser ran.
	for _, g := range gpus {
		if g.Vendor == "" || g.Model == "" {
			t.Errorf("adapter with empty vendor/model: %+v", g)
		}
	}
}

func TestDetectPlatformLive(t *testing.T) {
	p := &typesv1.CapabilityProfile{}
	if err := detectPlatform(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if p.RamTotalMb == 0 {
		t.Error("RamTotalMb = 0 after detectPlatform")
	}
	if p.CpuModel == "" {
		t.Error("CpuModel empty after detectPlatform")
	}
}

func TestPickCPUModel(t *testing.T) {
	cases := []struct{ reg, env, want string }{
		{"  Intel(R) Core(TM) i9-13900K   ", "Intel64 Family 6", "Intel(R) Core(TM) i9-13900K"},
		{"AMD Ryzen 9 7950X 16-Core Processor            ", "", "AMD Ryzen 9 7950X 16-Core Processor"},
		{"", "Intel64 Family 6 Model 154 Stepping 3, GenuineIntel", "Intel64 Family 6 Model 154 Stepping 3, GenuineIntel"},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := pickCPUModel(c.reg, c.env); got != c.want {
			t.Errorf("pickCPUModel(%q, %q) = %q, want %q", c.reg, c.env, got, c.want)
		}
	}
}

const (
	// ConvertTo-Json of a laptop with an Intel iGPU and an NVIDIA dGPU.
	videoControllersTwo = `[{"Name":"Intel(R) Iris(R) Xe Graphics","AdapterRAM":1073741824,"DriverVersion":"31.0.101.4502","PNPDeviceID":"PCI\\VEN_8086&DEV_46A6&SUBSYS_0A5A1028&REV_0C\\3&11583659&0&10"},{"Name":"NVIDIA GeForce RTX 4070 Laptop GPU","AdapterRAM":4293918720,"DriverVersion":"32.0.15.6094","PNPDeviceID":"PCI\\VEN_10DE&DEV_2820&SUBSYS_0A5A1028&REV_A1\\4&2A9DFE96&0&0008"}]`
	// A single adapter is emitted as a bare object; virtual adapters are dropped.
	videoControllerHyperV = `{"Name":"Microsoft Hyper-V Video","AdapterRAM":null,"DriverVersion":"10.0.20348.1","PNPDeviceID":"VMBUS\\{DA0A7802-E377-4AAA-8E77-0558EB1073F8}\\{5620E0C7-8062-4DCE-AEB7-520C7EF76171}"}`
	videoControllerAMD    = `{"Name":"AMD Radeon RX 7900 XTX","AdapterRAM":4293918720,"DriverVersion":"31.0.24027.1012","PNPDeviceID":"PCI\\VEN_1002&DEV_744C&SUBSYS_0E3A1002&REV_C8\\6&2C5C6A4&0&00000019"}`
)

func TestParseVideoControllers(t *testing.T) {
	gpus, err := parseVideoControllers([]byte(videoControllersTwo))
	if err != nil {
		t.Fatal(err)
	}
	if len(gpus) != 2 {
		t.Fatalf("gpus = %d, want 2: %+v", len(gpus), gpus)
	}
	if gpus[0].Vendor != "intel" || gpus[0].Model != "Intel(R) Iris(R) Xe Graphics" || gpus[0].DriverVersion != "31.0.101.4502" {
		t.Errorf("gpu[0] = %+v", gpus[0])
	}
	if gpus[1].Vendor != "nvidia" || gpus[1].Model != "NVIDIA GeForce RTX 4070 Laptop GPU" {
		t.Errorf("gpu[1] = %+v", gpus[1])
	}
	for _, g := range gpus {
		if g.VramMb != 0 || g.Accel != "" {
			t.Errorf("CIM adapters must carry no VRAM/accel (AdapterRAM caps at 4 GB): %+v", g)
		}
	}

	if gpus, err := parseVideoControllers([]byte(videoControllerHyperV)); err != nil || len(gpus) != 0 {
		t.Errorf("Hyper-V adapter: gpus=%v err=%v, want none", gpus, err)
	}
	gpus, err = parseVideoControllers([]byte(videoControllerAMD))
	if err != nil || len(gpus) != 1 || gpus[0].Vendor != "amd" {
		t.Errorf("AMD adapter: gpus=%v err=%v", gpus, err)
	}
	if gpus, err := parseVideoControllers([]byte("  \r\n")); err != nil || gpus != nil {
		t.Errorf("empty output: gpus=%v err=%v", gpus, err)
	}
	if _, err := parseVideoControllers([]byte("not json")); err == nil {
		t.Error("expected a parse error for garbage")
	}
}

func TestAdapterVendor(t *testing.T) {
	cases := []struct{ name, pnp, want string }{
		{"NVIDIA GeForce RTX 4090", `PCI\VEN_10DE&DEV_2684`, "nvidia"},
		{"Unknown", `pci\ven_10de&dev_2684`, "nvidia"},
		{"AMD Radeon(TM) Graphics", `PCI\VEN_1002&DEV_164E`, "amd"},
		{"Radeon RX 6800", "", "amd"},
		{"Intel(R) UHD Graphics 770", `PCI\VEN_8086&DEV_4680`, "intel"},
		{"Microsoft Basic Display Adapter", `ROOT\BasicDisplay\0000`, ""},
		{"Microsoft Remote Display Adapter", `SWD\REMOTEDISPLAYENUM\RDPIDD_INDIRECTDISPLAY`, ""},
	}
	for _, c := range cases {
		if got := adapterVendor(c.name, c.pnp); got != c.want {
			t.Errorf("adapterVendor(%q, %q) = %q, want %q", c.name, c.pnp, got, c.want)
		}
	}
}
