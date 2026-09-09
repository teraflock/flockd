package hardware

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
)

func TestAccelChain(t *testing.T) {
	cpu := cpuAccel()
	gpu := func(vendor, accel string) *typesv1.CapabilityProfile {
		return &typesv1.CapabilityProfile{Gpus: []*typesv1.GpuInfo{{Vendor: vendor, Model: vendor, Accel: accel, VramMb: 24560}}}
	}
	cases := []struct {
		name   string
		p      *typesv1.CapabilityProfile
		vulkan map[string]bool
		want   []string
	}{
		{"nvidia, cuda only", gpu("nvidia", "cuda12"), nil, []string{"cuda12", cpu}},
		{"nvidia with vulkan", gpu("nvidia", "cuda12"), map[string]bool{"nvidia": true}, []string{"cuda12", "vulkan", cpu}},
		{"amd with vulkan", gpu("amd", "rocm"), map[string]bool{"amd": true, "intel": true}, []string{"rocm", "vulkan", cpu}},
		{"amd, radv missing", gpu("amd", "rocm"), map[string]bool{"intel": true}, []string{"rocm", cpu}},
		{"intel arc: vulkan is the only GPU lane", gpu("intel", ""), map[string]bool{"intel": true}, []string{"vulkan", cpu}},
		{"intel without vulkan", gpu("intel", ""), nil, []string{cpu}},
		{"unknown vendor without vulkan", gpu("matrox", "cpu"), map[string]bool{"amd": true}, []string{cpu}},
		{"vendor-agnostic: apple with an injected vulkan lane", gpu("apple", "metal"), map[string]bool{"apple": true}, []string{"metal", "vulkan", cpu}},
		{"cpu-only fallback entry", gpu("none", cpu), map[string]bool{"amd": true}, []string{cpu}},
		{"empty profile", &typesv1.CapabilityProfile{}, nil, []string{cpu}},
		{"two gpus, dedup", &typesv1.CapabilityProfile{Gpus: []*typesv1.GpuInfo{
			{Vendor: "amd", Accel: "rocm"}, {Vendor: "amd", Accel: "rocm"}, {Vendor: "intel", Accel: ""},
		}}, map[string]bool{"amd": true, "intel": true}, []string{"rocm", "vulkan", cpu}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := accelChain(c.p, c.vulkan); !reflect.DeepEqual(got, c.want) {
				t.Errorf("accelChain = %v, want %v", got, c.want)
			}
		})
	}
	// The apple case only shows the chain is vendor-agnostic: vulkan_other.go
	// returns nil off Linux, so AccelPreference never adds vulkan on macOS.
}

func TestAccelPreferenceHeadIsBestAccel(t *testing.T) {
	for _, p := range []*typesv1.CapabilityProfile{
		{Gpus: []*typesv1.GpuInfo{{Vendor: "nvidia", Accel: "cuda12"}}},
		{Gpus: []*typesv1.GpuInfo{{Vendor: "amd", Accel: "rocm"}}},
		{Gpus: []*typesv1.GpuInfo{{Vendor: "none", Model: "cpu", Accel: cpuAccel()}}},
		{},
	} {
		chain := AccelPreference(p)
		if len(chain) == 0 || chain[len(chain)-1] != cpuAccel() {
			t.Errorf("chain %v must end in the cpu build", chain)
		}
		if BestAccel(p) != chain[0] {
			t.Errorf("BestAccel = %q, chain = %v", BestAccel(p), chain)
		}
	}
}

func vulkanFixture(t *testing.T, name string) vulkanProbe {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("testdata", "vulkan", name))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return vulkanProbe{root: root, libDirs: linuxVulkanLibDirs, icdDirs: linuxVulkanICDDirs}
}

func TestVulkanProbe(t *testing.T) {
	cases := map[string]map[string]bool{
		// Mesa installs radv + anv + lavapipe; lavapipe is software and a
		// broken manifest is skipped.
		"mesa":          {"amd": true, "intel": true},
		"nvidia":        {"nvidia": true},
		"amdvlk":        {"amd": true},
		"software-only": {},
		"no-loader":     nil,
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			got := vulkanFixture(t, name).vendors()
			if (got == nil) != (want == nil) || !reflect.DeepEqual(got, want) && !(len(got) == 0 && len(want) == 0) {
				t.Errorf("vendors = %#v, want %#v", got, want)
			}
		})
	}
	t.Run("missing root", func(t *testing.T) {
		p := vulkanProbe{root: filepath.Join(t.TempDir(), "nope"), libDirs: linuxVulkanLibDirs, icdDirs: linuxVulkanICDDirs}
		if got := p.vendors(); got != nil {
			t.Errorf("vendors = %#v, want nil", got)
		}
	})
	t.Run("LD_LIBRARY_PATH loader", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, vulkanLoaderSO), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("LD_LIBRARY_PATH", dir)
		p := newLinuxVulkanProbe(vulkanFixture(t, "no-loader").root)
		if got := p.vendors(); !got["amd"] {
			t.Errorf("vendors = %#v, want amd via the LD_LIBRARY_PATH loader", got)
		}
	})
}

func TestICDVendor(t *testing.T) {
	cases := map[string]string{
		"libvulkan_radeon.so":                     "amd",
		"/usr/lib/x86_64-linux-gnu/amdvlk64.so":   "amd",
		"libGLX_nvidia.so.0":                      "nvidia",
		"libvulkan_intel.so":                      "intel",
		"libvulkan_intel_hasvk.so":                "intel",
		"libvulkan_lvp.so":                        "",
		"libvulkan_virtio.so":                     "",
		"":                                        "",
		"/opt/amdgpu-pro/lib/x86_64/amdvlkpro.so": "amd",
	}
	for in, want := range cases {
		if got := icdVendor(in); got != want {
			t.Errorf("icdVendor(%q) = %q, want %q", in, got, want)
		}
	}
}
