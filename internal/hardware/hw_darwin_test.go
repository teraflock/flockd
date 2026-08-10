//go:build darwin

package hardware

import "testing"

const appleSiliconFixture = `{
  "SPDisplaysDataType": [
    {
      "_name": "Apple M2 Max",
      "sppci_model": "Apple M2 Max",
      "spdisplays_vendor": "sppci_vendor_Apple",
      "sppci_cores": "38",
      "spdisplays_mtlgpufamilysupport": "spdisplays_metal3"
    }
  ]
}`

const amdFixture = `{
  "SPDisplaysDataType": [
    {
      "_name": "Radeon Pro 5500M",
      "sppci_model": "AMD Radeon Pro 5500M",
      "spdisplays_vendor": "sppci_vendor_AMD",
      "spdisplays_vram": "8 GB",
      "spdisplays_mtlgpufamilysupport": "spdisplays_metal2"
    }
  ]
}`

func TestParseSPDisplaysAppleSilicon(t *testing.T) {
	gpus, err := parseSPDisplays([]byte(appleSiliconFixture), 65536)
	if err != nil {
		t.Fatal(err)
	}
	if len(gpus) != 1 {
		t.Fatalf("gpus = %d", len(gpus))
	}
	g := gpus[0]
	if g.Vendor != "apple" || !g.UnifiedMemory || g.Accel != "metal" {
		t.Errorf("unexpected gpu: %+v", g)
	}
	if g.VramMb != 65536 {
		t.Errorf("unified vram = %d, want system ram", g.VramMb)
	}
}

func TestParseSPDisplaysDiscreteAMD(t *testing.T) {
	gpus, err := parseSPDisplays([]byte(amdFixture), 32768)
	if err != nil {
		t.Fatal(err)
	}
	g := gpus[0]
	if g.Vendor != "amd" || g.UnifiedMemory {
		t.Errorf("unexpected gpu: %+v", g)
	}
	if g.VramMb != 8192 {
		t.Errorf("vram = %d, want 8192", g.VramMb)
	}
}

func TestParseVRAMMB(t *testing.T) {
	cases := map[string]uint64{"8 GB": 8192, "1536 MB": 1536, "": 0, "n/a": 0}
	for in, want := range cases {
		if got := parseVRAMMB(in); got != want {
			t.Errorf("parseVRAMMB(%q) = %d, want %d", in, got, want)
		}
	}
}
