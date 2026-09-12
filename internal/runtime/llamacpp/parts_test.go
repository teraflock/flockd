package llamacpp

import (
	"slices"
	"testing"

	rt "github.com/teraflock/flockd/internal/runtime"
)

// Admission hands the adapter a planned layout (flockd#46); without one
// the adapter keeps its own resolution, so callers that do not plan
// (tera's preflight, older tests) are unchanged.
func TestServerArgsHonourThePlannedLayout(t *testing.T) {
	a := &Adapter{Accel: "metal", MaxContext: 16384}
	arg := func(args []string, flag string) string {
		if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
			return args[i+1]
		}
		return ""
	}
	m := rt.ModelSpec{ID: "m", Path: "/models/m.gguf", ContextLength: 131072}

	unplanned := a.serverArgs(m, rt.ResourceBudget{MaxConcurrent: 2}, 1)
	if arg(unplanned, "--parallel") != "2" || arg(unplanned, "--ctx-size") != "16384" {
		t.Fatalf("unplanned: --parallel %s --ctx-size %s", arg(unplanned, "--parallel"), arg(unplanned, "--ctx-size"))
	}

	planned := a.serverArgs(m, rt.ResourceBudget{MaxConcurrent: 2, Slots: 12, ContextTokens: 12 * 8192}, 1)
	if arg(planned, "--parallel") != "12" || arg(planned, "--ctx-size") != "98304" {
		t.Fatalf("planned: --parallel %s --ctx-size %s", arg(planned, "--parallel"), arg(planned, "--ctx-size"))
	}
}

func TestServerArgsPassMmprojAndSizeTheWholeSet(t *testing.T) {
	a := &Adapter{Accel: "metal", VRAMMB: 16 * 1024}
	res := rt.ResourceBudget{MaxConcurrent: 2, MaxVRAMPercent: 80}

	plain := a.serverArgs(rt.ModelSpec{ID: "m", Path: "/models/m.gguf"}, res, 1234)
	if slices.Contains(plain, "--mmproj") {
		t.Fatalf("--mmproj without a sidecar: %v", plain)
	}
	if i := slices.Index(plain, "-m"); i < 0 || plain[i+1] != "/models/m.gguf" {
		t.Fatalf("-m missing: %v", plain)
	}

	vision := a.serverArgs(rt.ModelSpec{
		ID: "v", Path: "/models/v/v-00001-of-00003.gguf", MmprojPath: "/models/v/mmproj-F16.gguf",
	}, res, 1234)
	if i := slices.Index(vision, "--mmproj"); i < 0 || vision[i+1] != "/models/v/mmproj-F16.gguf" {
		t.Fatalf("--mmproj not passed: %v", vision)
	}
	if i := slices.Index(vision, "-m"); vision[i+1] != "/models/v/v-00001-of-00003.gguf" {
		t.Fatalf("part 1 is what llama-server gets: %v", vision)
	}

	// Offload is budgeted on the whole artifact, not a stat of part 1
	// (which does not even exist here): 40 GB does not fit 80% of 16 GB.
	huge := a.serverArgs(rt.ModelSpec{ID: "h", Path: "/nonexistent/h-00001-of-00005.gguf", SizeBytes: 40 << 30}, res, 1)
	if i := slices.Index(huge, "--n-gpu-layers"); i < 0 || huge[i+1] == "999" {
		t.Fatalf("full offload of a 40 GB set into 16 GB: %v", huge)
	}
	small := a.serverArgs(rt.ModelSpec{ID: "s", Path: "/nonexistent/s.gguf", SizeBytes: 4 << 30}, res, 1)
	if i := slices.Index(small, "--n-gpu-layers"); i < 0 || small[i+1] != "999" {
		t.Fatalf("4 GB should fully offload: %v", small)
	}
}
