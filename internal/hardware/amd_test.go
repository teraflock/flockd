package hardware

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
)

// The fixture trees under testdata/amd mirror /sys/class/drm and
// /sys/bus/pci as the amdgpu driver populates them (plain directories
// where sysfs has symlinks; the walker only reads through them). None of
// them was captured on real hardware — flockd#20 was written without an
// AMD box — so a real box should diff `tera status` against what these
// tests assert.
//
// PCI slot directories (0000:03:00.0) cannot be checked out on Windows —
// NTFS forbids ':' in names, and one such path fails the whole
// windows-latest checkout — so they are committed with '_' in place of
// ':' and fixture() copies the tree into a temp dir with the real names.

var pciSlotDirRe = regexp.MustCompile(`^[0-9a-f]{4}_[0-9a-f]{2}_[0-9a-f]{2}\.[0-9]$`)

func fixture(t *testing.T, name string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("sysfs fixture: PCI slot names contain ':' which NTFS cannot hold")
	}
	src, err := filepath.Abs(filepath.Join("testdata", "amd", name))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	root := t.TempDir()
	err = filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		parts := strings.Split(rel, string(filepath.Separator))
		for i, p := range parts {
			if pciSlotDirRe.MatchString(p) {
				parts[i] = strings.ReplaceAll(p, "_", ":")
			}
		}
		dst := filepath.Join(root, filepath.Join(parts...))
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
	if err != nil {
		t.Fatalf("copy fixture %s: %v", name, err)
	}
	return root
}

func readFixture(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", rel))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAMDSysfsWalkerSkipsNonAMDConnectorsAndRenderNodes(t *testing.T) {
	sys := amdSysfsGPUs(fixture(t, "rx7900xtx"))
	if len(sys) != 2 {
		t.Fatalf("sysfs gpus = %+v, want 2 (card1, card2)", sys)
	}
	g := sys[0]
	if g.Card != "card1" || g.DeviceID != "0x744c" || g.PCISlot != "0000:03:00.0" || g.SubsysID != "1002:0e3b" {
		t.Errorf("card1 = %+v", g)
	}
	if g.VRAMMB != 24560 {
		t.Errorf("card1 vram = %d MB, want 24560", g.VRAMMB)
	}
	if sys[1].Card != "card2" || sys[1].SubsysID != "1eae:7901" {
		t.Errorf("card2 = %+v", sys[1])
	}
}

func TestAMDSysfsOnly(t *testing.T) {
	root := fixture(t, "rx7900xtx")
	gpus := buildAMDGPUs(root, amdSysfsGPUs(root), rocmSMIInfo{}, "6.8.0-45-generic")
	if len(gpus) != 2 {
		t.Fatalf("gpus = %d, want 2", len(gpus))
	}
	want := []*typesv1.GpuInfo{
		{Vendor: "amd", Model: "Radeon RX 7900 XTX", VramMb: 24560, DriverVersion: "amdgpu (kernel 6.8.0-45-generic)", Accel: "rocm"},
		// Subsystem 1eae:7901 is not in pci.ids: silicon family name.
		{Vendor: "amd", Model: "Navi 31 [Radeon RX 7900 XT/7900 XTX/7900M]", VramMb: 24560, DriverVersion: "amdgpu (kernel 6.8.0-45-generic)", Accel: "rocm"},
	}
	for i := range want {
		assertGPU(t, gpus[i], want[i])
	}
	if unbound := amdUnboundPCI(root, amdSysfsGPUs(root)); len(unbound) != 0 {
		t.Errorf("unbound = %v, want none (both AMD GPUs have drm nodes; the HDMI audio function is not display class)", unbound)
	}
}

func TestAMDWithRocmSMI(t *testing.T) {
	root := fixture(t, "rx7900xtx")
	smi, err := parseRocmSMI(readFixture(t, "rocm-smi/rocm6-two-cards.json"))
	if err != nil {
		t.Fatal(err)
	}
	gpus := buildAMDGPUs(root, amdSysfsGPUs(root), smi, "6.8.0-45-generic")
	if len(gpus) != 2 {
		t.Fatalf("gpus = %d, want 2", len(gpus))
	}
	// pci.ids subsystem still beats rocm-smi's series for card1; card2 has
	// no subsystem entry so rocm-smi's series (matched by PCI bus, which
	// rocm-smi prints upper-case) wins over the pci.ids family name.
	assertGPU(t, gpus[0], &typesv1.GpuInfo{Vendor: "amd", Model: "Radeon RX 7900 XTX", VramMb: 24560, DriverVersion: "6.7.0", Accel: "rocm"})
	assertGPU(t, gpus[1], &typesv1.GpuInfo{Vendor: "amd", Model: "Radeon RX 7900 XT", VramMb: 24560, DriverVersion: "6.7.0", Accel: "rocm"})
}

func TestAMDModuleVersionBeatsRocmSMI(t *testing.T) {
	root := fixture(t, "mi210")
	smi := rocmSMIInfo{DriverVersion: "6.2.1"}
	gpus := buildAMDGPUs(root, amdSysfsGPUs(root), smi, "5.15.0")
	if len(gpus) != 1 {
		t.Fatalf("gpus = %d, want 1", len(gpus))
	}
	// product_name from sysfs wins; no pci.ids on this box; DKMS version
	// file beats rocm-smi's figure.
	assertGPU(t, gpus[0], &typesv1.GpuInfo{Vendor: "amd", Model: "AMD Instinct MI210", VramMb: 65520, DriverVersion: "6.7.0", Accel: "rocm"})
}

func TestAMDAPUBelowThresholdIsSkipped(t *testing.T) {
	root := fixture(t, "apu")
	sys := amdSysfsGPUs(root)
	if len(sys) != 1 || sys[0].VRAMMB != 512 || sys[0].DeviceID != "0x15bf" {
		t.Fatalf("sysfs = %+v, want one 512 MB Phoenix iGPU", sys)
	}
	if gpus := buildAMDGPUs(root, sys, rocmSMIInfo{}, "6.8.0"); len(gpus) != 0 {
		t.Errorf("gpus = %+v, want none: %d MB carveout is below the %d MB threshold", gpus, sys[0].VRAMMB, minAMDVRAMMB)
	}
}

func TestAMDNoDriverBound(t *testing.T) {
	root := fixture(t, "nodriver")
	sys := amdSysfsGPUs(root)
	if len(sys) != 0 {
		t.Fatalf("sysfs = %+v, want none (no drm card nodes)", sys)
	}
	unbound := amdUnboundPCI(root, sys)
	if len(unbound) != 1 || unbound[0] != "0000:03:00.0" {
		t.Errorf("unbound = %v, want [0000:03:00.0] (the .1 function is HDMI audio)", unbound)
	}
	if gpus := buildAMDGPUs(root, sys, rocmSMIInfo{}, "6.8.0"); len(gpus) != 0 {
		t.Errorf("gpus = %+v, want none", gpus)
	}
}

func TestAMDMissingFixtureRootIsEmpty(t *testing.T) {
	root := filepath.Join(t.TempDir(), "nope")
	if sys := amdSysfsGPUs(root); len(sys) != 0 {
		t.Errorf("sysfs = %+v", sys)
	}
	if u := amdUnboundPCI(root, nil); len(u) != 0 {
		t.Errorf("unbound = %v", u)
	}
	if v := amdModuleVersion(root); v != "" {
		t.Errorf("module version = %q", v)
	}
}

func TestPCIIDsName(t *testing.T) {
	root := fixture(t, "rx7900xtx")
	cases := []struct {
		dev, sub, wantSub, wantDev string
	}{
		{"0x744c", "1002:0e3b", "Radeon RX 7900 XTX", "Navi 31 [Radeon RX 7900 XT/7900 XTX/7900M]"},
		{"0x744C", "1DA2:471E", "PULSE RX 7900 XTX", "Navi 31 [Radeon RX 7900 XT/7900 XTX/7900M]"},
		{"0x744c", "1eae:7901", "", "Navi 31 [Radeon RX 7900 XT/7900 XTX/7900M]"},
		{"0x744c", "", "", "Navi 31 [Radeon RX 7900 XT/7900 XTX/7900M]"},
		{"0x15bf", "17aa:50a5", "", "Phoenix1"},
		// Last device of the 1002 block: the walker must not spill into 10de.
		{"0x747e", "1002:0e3c", "Radeon RX 7800 XT", "Navi 32 [Radeon RX 7700 XT / 7800 XT]"},
		{"0x2684", "", "", ""}, // an NVIDIA id is not looked up under 1002
		{"0xffff", "", "", ""},
	}
	for _, c := range cases {
		sub, dev := pciIDsName(root, c.dev, c.sub)
		if sub != c.wantSub || dev != c.wantDev {
			t.Errorf("pciIDsName(%s, %s) = (%q, %q), want (%q, %q)", c.dev, c.sub, sub, dev, c.wantSub, c.wantDev)
		}
	}
	if sub, dev := pciIDsName(t.TempDir(), "0x744c", "1002:0e3b"); sub != "" || dev != "" {
		t.Errorf("no pci.ids: got (%q, %q)", sub, dev)
	}
}

func TestParseRocmSMI(t *testing.T) {
	t.Run("rocm6", func(t *testing.T) {
		info, err := parseRocmSMI(readFixture(t, "rocm-smi/rocm6-two-cards.json"))
		if err != nil {
			t.Fatal(err)
		}
		if info.DriverVersion != "6.7.0" {
			t.Errorf("driver = %q", info.DriverVersion)
		}
		if len(info.Cards) != 2 {
			t.Fatalf("cards = %+v", info.Cards)
		}
		c := info.Cards[0]
		if c.Bus != "0000:03:00.0" || c.DeviceID != "0x744c" || c.VRAMMB != 24560 ||
			c.Series != "Navi 31 [Radeon RX 7900 XT/7900 XTX/7900M]" {
			t.Errorf("card0 = %+v", c)
		}
		if info.Cards[1].Bus != "0000:0a:00.0" || info.Cards[1].Series != "Radeon RX 7900 XT" {
			t.Errorf("card1 = %+v", info.Cards[1])
		}
	})
	t.Run("rocm5 lowercase keys with preamble", func(t *testing.T) {
		info, err := parseRocmSMI(readFixture(t, "rocm-smi/rocm5-warning-preamble.json"))
		if err != nil {
			t.Fatal(err)
		}
		if info.DriverVersion != "6.3.6" || len(info.Cards) != 1 {
			t.Fatalf("info = %+v", info)
		}
		c := info.Cards[0]
		if c.Bus != "" || c.Series != "Navi 31 [Radeon RX 7900 XT/7900 XTX]" || c.VRAMMB != 24560 {
			t.Errorf("card0 = %+v", c)
		}
	})
	t.Run("garbage", func(t *testing.T) {
		if _, err := parseRocmSMI(readFixture(t, "rocm-smi/garbage.txt")); err == nil {
			t.Error("expected error for non-JSON output")
		}
		if _, err := parseRocmSMI([]byte("{not json")); err == nil {
			t.Error("expected error for broken JSON")
		}
	})
	t.Run("card ordering is numeric", func(t *testing.T) {
		raw := []byte(`{"card10": {"PCI Bus": "0000:0c:00.0"}, "card2": {"PCI Bus": "0000:0b:00.0"}, "card0": {"PCI Bus": "0000:0a:00.0"}}`)
		info, err := parseRocmSMI(raw)
		if err != nil {
			t.Fatal(err)
		}
		if len(info.Cards) != 3 || info.Cards[0].Bus != "0000:0a:00.0" || info.Cards[1].Bus != "0000:0b:00.0" || info.Cards[2].Bus != "0000:0c:00.0" {
			t.Errorf("cards = %+v", info.Cards)
		}
	})
}

func TestMatchRocmCard(t *testing.T) {
	byBus := []rocmSMICard{{Bus: "0000:0a:00.0", Series: "second"}, {Bus: "0000:03:00.0", Series: "first"}}
	if c := matchRocmCard(byBus, amdSysfsGPU{PCISlot: "0000:03:00.0"}, 0, 2); c.Series != "first" {
		t.Errorf("by bus = %+v", c)
	}
	// Old rocm-smi without --showbus: positional only when the counts agree.
	positional := []rocmSMICard{{Series: "a"}, {Series: "b"}}
	if c := matchRocmCard(positional, amdSysfsGPU{PCISlot: "0000:0a:00.0"}, 1, 2); c.Series != "b" {
		t.Errorf("positional = %+v", c)
	}
	if c := matchRocmCard(positional, amdSysfsGPU{}, 0, 3); c.Series != "" {
		t.Errorf("count mismatch should not pair, got %+v", c)
	}
	if c := matchRocmCard(nil, amdSysfsGPU{}, 0, 1); c.Series != "" {
		t.Errorf("no cards should not pair, got %+v", c)
	}
}

func TestPickLinuxGPUsPrefersNvidia(t *testing.T) {
	nv := []*typesv1.GpuInfo{{Vendor: "nvidia", Model: "GeForce RTX 4090", Accel: "cuda12"}}
	amdCalled := false
	amd := func() []*typesv1.GpuInfo {
		amdCalled = true
		return []*typesv1.GpuInfo{{Vendor: "amd", Model: "Radeon RX 7900 XTX", Accel: "rocm"}}
	}
	got := pickLinuxGPUs(nv, amd)
	if len(got) != 1 || got[0].Vendor != "nvidia" || amdCalled {
		t.Errorf("mixed box: got %+v (amd probed: %v), want the NVIDIA list without probing AMD", got, amdCalled)
	}
	got = pickLinuxGPUs(nil, amd)
	if len(got) != 1 || got[0].Vendor != "amd" || !amdCalled {
		t.Errorf("no nvidia: got %+v", got)
	}
}

func assertGPU(t *testing.T, got, want *typesv1.GpuInfo) {
	t.Helper()
	if got.GetVendor() != want.GetVendor() || got.GetModel() != want.GetModel() ||
		got.GetVramMb() != want.GetVramMb() || got.GetDriverVersion() != want.GetDriverVersion() ||
		got.GetAccel() != want.GetAccel() || got.GetUnifiedMemory() != want.GetUnifiedMemory() {
		t.Errorf("gpu = %+v\n  want %+v", got, want)
	}
}
