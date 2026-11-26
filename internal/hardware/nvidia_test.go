package hardware

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
)

func TestParseNvidiaSMI(t *testing.T) {
	rows := parseNvidiaSMI("NVIDIA GeForce RTX 4090, 24564, 560.94\nNVIDIA RTX A2000, 6144, 560.94\n\n")
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].CapKnown || rows[1].CapKnown {
		t.Errorf("three-column output must leave the capability unknown: %+v", rows)
	}
	gpus, warnings := nvidiaGPUInfos(rows, false, "linux")
	if len(gpus) != 2 || len(warnings) != 0 {
		t.Fatalf("gpus = %d, warnings = %v", len(gpus), warnings)
	}
	g := gpus[0]
	if g.Vendor != "nvidia" || g.Model != "NVIDIA GeForce RTX 4090" || g.VramMb != 24564 ||
		g.DriverVersion != "560.94" || g.Accel != "cuda12" {
		t.Errorf("gpu[0] = %+v", g)
	}
	if gpus[1].VramMb != 6144 {
		t.Errorf("gpu[1].VramMb = %d", gpus[1].VramMb)
	}
	if got := parseNvidiaSMI(""); len(got) != 0 {
		t.Errorf("empty output -> %d gpus", len(got))
	}
	if got := parseNvidiaSMI("No devices were found\n"); len(got) != 0 {
		t.Errorf("error text -> %d gpus", len(got))
	}
	if got, w := nvidiaGPUInfos(nil, true, "linux"); got != nil || w != nil {
		t.Errorf("no rows -> %v, %v; want nil, nil", got, w)
	}

	// Four columns, Windows line endings: the capability is the last field.
	rows = parseNvidiaSMI("NVIDIA GeForce GTX 1080 Ti, 11264, 536.23, 6.1\r\nNVIDIA GeForce RTX 4090, 24564, 536.23, 8.9\r\n")
	want := []nvidiaGPU{
		{Name: "NVIDIA GeForce GTX 1080 Ti", VRAMMB: 11264, DriverVersion: "536.23", CapMajor: 6, CapMinor: 1, CapKnown: true},
		{Name: "NVIDIA GeForce RTX 4090", VRAMMB: 24564, DriverVersion: "536.23", CapMajor: 8, CapMinor: 9, CapKnown: true},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("rows = %+v\nwant   %+v", rows, want)
	}

	// A stray comma shifts the columns: the row is still reported (as it
	// always was) but nothing in it is read as a capability.
	rows = parseNvidiaSMI("NVIDIA Odd, Name, 8192, 536.23, 6.1\n")
	if len(rows) != 1 || rows[0].CapKnown {
		t.Errorf("five-column row = %+v, want one row with unknown capability", rows)
	}
}

func TestParseComputeCap(t *testing.T) {
	type parsed struct {
		major, minor int
		ok           bool
	}
	cases := map[string]parsed{
		"6.1":             {6, 1, true},
		" 6.1":            {6, 1, true},
		"7.0\r":           {7, 0, true},
		"7.5":             {7, 5, true},
		"8.9":             {8, 9, true},
		"12.0":            {12, 0, true},
		"3.5":             {3, 5, true},
		"":                {},
		" ":               {},
		"[N/A]":           {},
		"N/A":             {},
		"[Not Supported]": {},
		"[Unknown Error]": {},
		"7":               {},
		"7.":              {},
		".5":              {},
		".":               {},
		"6.1.0":           {},
		"6,1":             {},
		"-6.1":            {},
		"+6.1":            {},
		"6.-1":            {},
		"6. 1":            {},
		"6.1 x":           {},
		"v6.1":            {},
		"0x6.1":           {},
		"6e0.1":           {},
		"0.0":             {},
		"0.9":             {},
		"1234.5":          {},
		"6.1234":          {},
		"６.１":             {}, // full-width digits
		"six.one":         {},
	}
	for in, want := range cases {
		major, minor, ok := parseComputeCap(in)
		if got := (parsed{major, minor, ok}); got != want {
			t.Errorf("parseComputeCap(%q) = %+v, want %+v", in, got, want)
		}
	}
}

// The compute-capability gate, end to end from nvidia-smi text to the
// profile entries and the accelerator chain the runtime fetcher walks.
func TestNvidiaComputeCapLanes(t *testing.T) {
	cpu := cpuAccel()
	withVulkan := map[string]bool{"nvidia": true}
	cases := []struct {
		name      string
		smi       string
		vulkan    map[string]bool
		wantAccel []string // per GPU, in nvidia-smi order
		wantChain []string
		wantWarn  []string // one substring per expected warning, in order
	}{
		{
			name:      "pascal 6.1 demoted to vulkan",
			smi:       "NVIDIA GeForce GTX 1080 Ti, 11264, 560.94, 6.1\n",
			vulkan:    withVulkan,
			wantAccel: []string{""},
			wantChain: []string{"vulkan", cpu},
			wantWarn:  []string{"NVIDIA GeForce GTX 1080 Ti has compute capability 6.1"},
		},
		{
			name:      "pascal 6.1 demoted to cpu without a vulkan driver",
			smi:       "NVIDIA GeForce GTX 1060 6GB, 6144, 560.94, 6.1\n",
			wantAccel: []string{""},
			wantChain: []string{cpu},
			wantWarn:  []string{"NVIDIA GeForce GTX 1060 6GB has compute capability 6.1"},
		},
		{
			name:      "volta 7.0 demoted",
			smi:       "Tesla V100-SXM2-16GB, 16384, 535.183.01, 7.0\n",
			vulkan:    withVulkan,
			wantAccel: []string{""},
			wantChain: []string{"vulkan", cpu},
			wantWarn:  []string{"Tesla V100-SXM2-16GB has compute capability 7.0"},
		},
		{
			name:      "maxwell 5.2 demoted",
			smi:       "NVIDIA GeForce GTX 980 Ti, 6144, 535.183.01, 5.2\n",
			wantAccel: []string{""},
			wantChain: []string{cpu},
			wantWarn:  []string{"compute capability 5.2"},
		},
		{
			name:      "turing 7.5 kept (the floor itself)",
			smi:       "NVIDIA GeForce GTX 1660 Ti, 6144, 560.94, 7.5\n",
			vulkan:    withVulkan,
			wantAccel: []string{"cuda12"},
			wantChain: []string{"cuda12", "vulkan", cpu},
		},
		{
			name:      "ampere 8.6 kept",
			smi:       "NVIDIA GeForce RTX 3060, 12288, 560.94, 8.6\n",
			wantAccel: []string{"cuda12"},
			wantChain: []string{"cuda12", cpu},
		},
		{
			name:      "ada 8.9 kept",
			smi:       "NVIDIA GeForce RTX 4090, 24564, 560.94, 8.9\n",
			vulkan:    withVulkan,
			wantAccel: []string{"cuda12"},
			wantChain: []string{"cuda12", "vulkan", cpu},
		},
		{
			name:      "blackwell 12.0 kept (two-digit major)",
			smi:       "NVIDIA GeForce RTX 5090, 32607, 572.16, 12.0\n",
			wantAccel: []string{"cuda12"},
			wantChain: []string{"cuda12", cpu},
		},
		{
			name:      "compute_cap not queried (fallback query) kept",
			smi:       "NVIDIA GeForce GTX 1080 Ti, 11264, 390.157\n",
			vulkan:    withVulkan,
			wantAccel: []string{"cuda12"},
			wantChain: []string{"cuda12", "vulkan", cpu},
		},
		{
			name:      "garbage capability kept",
			smi:       "NVIDIA GeForce GTX 1080 Ti, 11264, 560.94, banana\n",
			wantAccel: []string{"cuda12"},
			wantChain: []string{"cuda12", cpu},
		},
		{
			name:      "[N/A] capability kept",
			smi:       "NVIDIA GeForce GTX 1080 Ti, 11264, 560.94, [N/A]\n",
			wantAccel: []string{"cuda12"},
			wantChain: []string{"cuda12", cpu},
		},
		{
			name:      "empty capability kept",
			smi:       "NVIDIA GeForce GTX 1080 Ti, 11264, 560.94, \n",
			wantAccel: []string{"cuda12"},
			wantChain: []string{"cuda12", cpu},
		},
		{
			name:      "zero capability kept",
			smi:       "NVIDIA GeForce GTX 1080 Ti, 11264, 560.94, 0.0\n",
			wantAccel: []string{"cuda12"},
			wantChain: []string{"cuda12", cpu},
		},
		{
			name:      "mixed box, old card first: the new card stays on cuda",
			smi:       "NVIDIA GeForce GTX 1080, 8192, 560.94, 6.1\nNVIDIA GeForce RTX 4070, 12282, 560.94, 8.9\n",
			vulkan:    withVulkan,
			wantAccel: []string{"cuda12", "cuda12"},
			wantChain: []string{"cuda12", "vulkan", cpu},
			wantWarn:  []string{"NVIDIA GeForce GTX 1080 has compute capability 6.1"},
		},
		{
			name:      "mixed box, new card first",
			smi:       "NVIDIA GeForce RTX 4070, 12282, 560.94, 8.9\nNVIDIA GeForce GTX 1080, 8192, 560.94, 6.1\n",
			wantAccel: []string{"cuda12", "cuda12"},
			wantChain: []string{"cuda12", cpu},
			wantWarn:  []string{"NVIDIA GeForce GTX 1080 has compute capability 6.1"},
		},
		{
			name:      "old card next to one of unknown capability: box stays on cuda",
			smi:       "NVIDIA GeForce GTX 1080, 8192, 560.94, 6.1\nNVIDIA Mystery, 16384, 560.94, [N/A]\n",
			wantAccel: []string{"cuda12", "cuda12"},
			wantChain: []string{"cuda12", cpu},
			wantWarn:  []string{"NVIDIA GeForce GTX 1080 has compute capability 6.1"},
		},
		{
			name:      "two old cards: the box is demoted, one warning each",
			smi:       "NVIDIA GeForce GTX 1080 Ti, 11264, 560.94, 6.1\nNVIDIA TITAN V, 12288, 560.94, 7.0\n",
			vulkan:    withVulkan,
			wantAccel: []string{"", ""},
			wantChain: []string{"vulkan", cpu},
			wantWarn:  []string{"NVIDIA GeForce GTX 1080 Ti has compute capability 6.1", "NVIDIA TITAN V has compute capability 7.0"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rows := parseNvidiaSMI(c.smi)
			gpus, warnings := nvidiaGPUInfos(rows, c.vulkan["nvidia"], "linux")
			if len(gpus) != len(c.wantAccel) || len(gpus) != len(rows) {
				t.Fatalf("gpus = %d, rows = %d, want %d", len(gpus), len(rows), len(c.wantAccel))
			}
			for i, g := range gpus {
				if g.Accel != c.wantAccel[i] {
					t.Errorf("gpu[%d] %s: Accel = %q, want %q", i, g.Model, g.Accel, c.wantAccel[i])
				}
				// Only the lane may change: identity and VRAM are reported
				// exactly as nvidia-smi gave them.
				if g.Vendor != "nvidia" || g.Model != rows[i].Name || g.VramMb != rows[i].VRAMMB ||
					g.VramMb == 0 || g.DriverVersion != rows[i].DriverVersion || g.UnifiedMemory {
					t.Errorf("gpu[%d] = %+v, row = %+v", i, g, rows[i])
				}
			}
			p := &typesv1.CapabilityProfile{Gpus: gpus}
			if got := accelChain(p, c.vulkan); !reflect.DeepEqual(got, c.wantChain) {
				t.Errorf("accelChain = %v, want %v", got, c.wantChain)
			}
			if len(warnings) != len(c.wantWarn) {
				t.Fatalf("warnings = %q, want %d", warnings, len(c.wantWarn))
			}
			for i, w := range warnings {
				if !strings.Contains(w, c.wantWarn[i]) ||
					!strings.Contains(w, "the CUDA runtime needs 7.5 or newer (RTX 20-series / GTX 16-series or newer)") {
					t.Errorf("warning[%d] = %q, want it to contain %q and the requirement", i, w, c.wantWarn[i])
				}
			}
		})
	}
}

// The exact operator-facing text for each outcome.
func TestNvidiaCapWarningText(t *testing.T) {
	const head = "nvidia gpu too old for the CUDA runtime: NVIDIA GeForce GTX 1080 Ti has compute capability 6.1, " +
		"the CUDA runtime needs 7.5 or newer (RTX 20-series / GTX 16-series or newer)"
	old := "NVIDIA GeForce GTX 1080 Ti, 11264, 560.94, 6.1\n"
	cases := []struct {
		name   string
		smi    string
		vulkan bool
		goos   string
		want   string
	}{
		{"vulkan available", old, true, "linux", head + "; falling back to the Vulkan runtime"},
		{"linux without vulkan", old, false, "linux", head + "; falling back to the CPU runtime (install the Vulkan loader and the NVIDIA Vulkan driver to serve on this GPU through the Vulkan runtime)"},
		{"windows", old, false, "windows", head + "; falling back to the CPU runtime"},
		{"mixed box", old + "NVIDIA GeForce RTX 4090, 24564, 560.94, 8.9\n", true, "linux", head + "; the node stays on the CUDA runtime for its other NVIDIA GPU, and this card cannot serve on it"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, warnings := nvidiaGPUInfos(parseNvidiaSMI(c.smi), c.vulkan, c.goos)
			if len(warnings) != 1 || warnings[0] != c.want {
				t.Errorf("warnings = %q\nwant      [%q]", warnings, c.want)
			}
		})
	}
}

// A driver that predates compute_cap fails the whole query; detection must
// fall back to the three-column query and treat the capability as unknown.
func TestQueryNvidiaSMIFallback(t *testing.T) {
	const base = "NVIDIA GeForce GTX 1080 Ti, 11264, 390.157\n"
	const withCap = "NVIDIA GeForce GTX 1080 Ti, 11264, 560.94, 6.1\n"
	const invalidField = "Field \"compute_cap\" is not a valid field to query.\n\n"
	errExit := errors.New("exit status 2")

	cases := []struct {
		name      string
		capOut    string
		capErr    error
		baseOut   string
		baseErr   error
		wantCalls []string
		wantRows  int
		wantKnown bool
	}{
		{"compute_cap supported: one query", withCap, nil, base, nil,
			[]string{nvidiaSMIFieldsWithCap}, 1, true},
		{"compute_cap rejected with an error exit", invalidField, errExit, base, nil,
			[]string{nvidiaSMIFieldsWithCap, nvidiaSMIFields}, 1, false},
		{"compute_cap rejected with a zero exit", invalidField, nil, base, nil,
			[]string{nvidiaSMIFieldsWithCap, nvidiaSMIFields}, 1, false},
		{"compute_cap query prints nothing", "", nil, base, nil,
			[]string{nvidiaSMIFieldsWithCap, nvidiaSMIFields}, 1, false},
		{"both queries fail: no nvidia gpus", "", errExit, "NVIDIA-SMI has failed\n", errExit,
			[]string{nvidiaSMIFieldsWithCap, nvidiaSMIFields}, 0, false},
		{"no devices", "No devices were found\n", nil, "No devices were found\n", nil,
			[]string{nvidiaSMIFieldsWithCap, nvidiaSMIFields}, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var calls []string
			rows := queryNvidiaSMI(func(fields string) ([]byte, error) {
				calls = append(calls, fields)
				if fields == nvidiaSMIFieldsWithCap {
					return []byte(c.capOut), c.capErr
				}
				return []byte(c.baseOut), c.baseErr
			})
			if !reflect.DeepEqual(calls, c.wantCalls) {
				t.Errorf("queries = %q, want %q", calls, c.wantCalls)
			}
			if len(rows) != c.wantRows {
				t.Fatalf("rows = %+v, want %d", rows, c.wantRows)
			}
			if c.wantRows == 0 {
				return
			}
			if rows[0].CapKnown != c.wantKnown {
				t.Errorf("CapKnown = %v, want %v", rows[0].CapKnown, c.wantKnown)
			}
			// A fallback answer always keeps the card on cuda12.
			gpus, warnings := nvidiaGPUInfos(rows, true, "linux")
			wantAccel := "cuda12"
			if c.wantKnown {
				wantAccel = ""
			}
			if gpus[0].Accel != wantAccel || (len(warnings) > 0) != c.wantKnown {
				t.Errorf("Accel = %q (want %q), warnings = %q", gpus[0].Accel, wantAccel, warnings)
			}
		})
	}
	if nvidiaSMIFields != "name,memory.total,driver_version" ||
		nvidiaSMIFieldsWithCap != "name,memory.total,driver_version,compute_cap" {
		t.Errorf("query fields changed: %q / %q", nvidiaSMIFields, nvidiaSMIFieldsWithCap)
	}
}

// nvidiaReport is what `tera doctor` shows: the driver's own facts, and
// for a card below the CUDA floor the same sentence detection logs.
func TestNvidiaReport(t *testing.T) {
	if got := nvidiaReport(nil, true, "linux"); got != nil {
		t.Errorf("no rows -> %v, want nil", got)
	}

	rows := parseNvidiaSMI("NVIDIA GeForce GTX 1080 Ti, 11264, 536.23, 6.1\nNVIDIA GeForce RTX 4090, 24564, 536.23, 8.9\nNVIDIA Quadro, 8192, 390.1\n")
	// Three columns on the last line: its capability is unknown.
	rows[2].CapKnown = false
	got := nvidiaReport(rows, false, "linux")
	_, warnings := nvidiaGPUInfos(rows, false, "linux")
	if len(got) != 3 || len(warnings) != 1 {
		t.Fatalf("report = %+v, warnings = %v", got, warnings)
	}
	old, modern, unknown := got[0], got[1], got[2]
	if old.Name != "NVIDIA GeForce GTX 1080 Ti" || old.DriverVersion != "536.23" || old.VRAMMB != 11264 ||
		old.ComputeCap != "6.1" || old.RunsCUDA || old.Warning != warnings[0] {
		t.Errorf("old card = %+v, want the logged warning %q", old, warnings[0])
	}
	if !strings.Contains(old.Warning, "stays on the CUDA runtime for its other NVIDIA GPU") {
		t.Errorf("mixed-box warning = %q", old.Warning)
	}
	if modern.ComputeCap != "8.9" || !modern.RunsCUDA || modern.Warning != "" {
		t.Errorf("modern card = %+v", modern)
	}
	if unknown.ComputeCap != "" || !unknown.RunsCUDA || unknown.Warning != "" {
		t.Errorf("card with no reported capability = %+v (unknown counts as able)", unknown)
	}

	// A box with only old cards: the fallback named in the warning follows
	// the Vulkan probe and the OS, exactly as at detection.
	only := parseNvidiaSMI("NVIDIA GeForce GTX 1080, 8192, 536.23, 6.1\n")
	for _, c := range []struct {
		vulkan bool
		goos   string
		want   string
	}{
		{true, "linux", "falling back to the Vulkan runtime"},
		{false, "linux", "install the Vulkan loader"},
		{false, "windows", "falling back to the CPU runtime"},
	} {
		r := nvidiaReport(only, c.vulkan, c.goos)
		_, w := nvidiaGPUInfos(only, c.vulkan, c.goos)
		if len(r) != 1 || r[0].RunsCUDA || r[0].Warning != w[0] || !strings.Contains(r[0].Warning, c.want) {
			t.Errorf("vulkan=%v %s: %+v, want %q", c.vulkan, c.goos, r, c.want)
		}
	}
}
