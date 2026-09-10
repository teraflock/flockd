package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// None of these fixtures was captured on real hardware — flockd#16 was
// written without an AMD box — so a real box should diff `tera status`'s
// memory.vram against rocm-smi / amdgpu_top.

func readFixture(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", rel))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAMDSysfsVRAMUsed(t *testing.T) {
	// 312852480 + 24117248 B over the two AMD cards; the NVIDIA card, the
	// connector node and the render node are skipped.
	mb, err := amdSysfsVRAMUsedMB(filepath.Join("testdata", "sysfs-amd"))
	if err != nil || mb != 321 {
		t.Fatalf("sysfs-amd = %d MB, %v; want 321", mb, err)
	}
	if _, err := amdSysfsVRAMUsedMB(filepath.Join("testdata", "sysfs-nvidia")); !errors.Is(err, errNoAMDSysfs) {
		t.Fatalf("nvidia-only tree: err = %v, want errNoAMDSysfs", err)
	}
	if _, err := amdSysfsVRAMUsedMB(filepath.Join(t.TempDir(), "nope")); !errors.Is(err, errNoAMDSysfs) {
		t.Fatalf("missing tree: err = %v, want errNoAMDSysfs", err)
	}

	// An AMD card whose counter is missing or unparsable is an error, not
	// an under-count: the sampler keeps its previous figure.
	root := t.TempDir()
	dev := filepath.Join(root, "sys", "class", "drm", "card0", "device")
	if err := os.MkdirAll(dev, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dev, "vendor"), []byte("0x1002\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := amdSysfsVRAMUsedMB(root); err == nil || errors.Is(err, errNoAMDSysfs) {
		t.Fatalf("missing counter: err = %v, want a read error", err)
	}
	if err := os.WriteFile(filepath.Join(dev, "mem_info_vram_used"), []byte("N/A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := amdSysfsVRAMUsedMB(root); err == nil || errors.Is(err, errNoAMDSysfs) {
		t.Fatalf("garbage counter: err = %v, want a parse error", err)
	}
}

func TestParseRocmSMIVRAMUsed(t *testing.T) {
	// 312852480 + 24117248 B = 321 MiB (summed in bytes, then rounded).
	if mb, err := parseRocmSMIVRAMUsed(readFixture(t, "rocm-smi/meminfo-vram-rocm6.json")); err != nil || mb != 321 {
		t.Fatalf("rocm6: %d MB, %v; want 321", mb, err)
	}
	// ROCm 5 prints a warning before the JSON; 2113929216 B = 2016 MiB.
	if mb, err := parseRocmSMIVRAMUsed(readFixture(t, "rocm-smi/meminfo-vram-rocm5-preamble.json")); err != nil || mb != 2016 {
		t.Fatalf("rocm5: %d MB, %v; want 2016", mb, err)
	}
	// Numeric JSON values, should a release stop quoting them.
	if mb, err := parseRocmSMIVRAMUsed([]byte(`{"card0": {"vram total used memory (b)": 1048576}}`)); err != nil || mb != 1 {
		t.Fatalf("numeric: %d MB, %v; want 1", mb, err)
	}
	// --showmemuse output carries percentages only: no usable figure.
	if _, err := parseRocmSMIVRAMUsed(readFixture(t, "rocm-smi/showmemuse-percent.json")); err == nil {
		t.Fatal("percent-only output accepted")
	}
	if _, err := parseRocmSMIVRAMUsed(readFixture(t, "rocm-smi/garbage.txt")); err == nil {
		t.Fatal("non-JSON output accepted")
	}
	if _, err := parseRocmSMIVRAMUsed([]byte(`{"card0": {"VRAM Total Used Memory (B)": "lots"}}`)); err == nil {
		t.Fatal("garbage figure accepted")
	}
}

func TestAMDVRAMUsedMBFallsBackToRocmSMI(t *testing.T) {
	// sysfs answers: rocm-smi is never consulted.
	old := amdSysfsRoot
	t.Cleanup(func() { amdSysfsRoot = old })
	amdSysfsRoot = filepath.Join("testdata", "sysfs-amd")
	t.Setenv("PATH", t.TempDir())
	if mb, err := AMDVRAMUsedMB(context.Background()); err != nil || mb != 321 {
		t.Fatalf("sysfs path: %d MB, %v", mb, err)
	}
	// No amdgpu card in sysfs and no rocm-smi on PATH: the sampler
	// disables itself rather than retrying every tick.
	amdSysfsRoot = filepath.Join("testdata", "sysfs-nvidia")
	if _, err := AMDVRAMUsedMB(context.Background()); !errors.Is(err, ErrNoGPUTool) {
		t.Fatalf("no sysfs, no tool: err = %v, want ErrNoGPUTool", err)
	}
}
