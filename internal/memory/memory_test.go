package memory

import (
	"os"
	"runtime"
	"testing"

	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
)

func TestEstimateMB(t *testing.T) {
	const gb = 1024 * MiB
	// 5 GB Q4 8B at 8k ctx, 2 slots: 5.75 GB weights + 16384 × 80 KB KV +
	// overhead — a little over 7 GB, which is what such a load measures.
	got := EstimateMB(5*gb, 0, 8192, 2)
	if got < 7000 || got > 7600 {
		t.Fatalf("EstimateMB(5GB, 8k, 2) = %d MB, want ~7.2 GB", got)
	}
	// Catalog min_ram_mb wins when larger.
	if got := EstimateMB(5*gb, 12288, 8192, 2); got != 12288 {
		t.Fatalf("min_ram_mb not honoured: %d", got)
	}
	// Bigger context costs more, and the catalog floor cannot hide it.
	if EstimateMB(5*gb, 0, 65536, 2) <= EstimateMB(5*gb, 0, 8192, 2) {
		t.Fatal("context length has no effect on the estimate")
	}
	// Unknown context falls back to DefaultContext, not zero KV.
	if EstimateMB(5*gb, 0, 0, 1) <= int64(5*gb*115/100/MiB)+runtimeOverheadMB {
		t.Fatal("zero context produced no KV term")
	}
}

func TestAutoBudget(t *testing.T) {
	unified := &typesv1.CapabilityProfile{RamTotalMb: 65536, Gpus: []*typesv1.GpuInfo{{Vendor: "apple", VramMb: 65536, UnifiedMemory: true}}}
	if got := AutoBudgetMB(unified, 80); got != 32768 {
		t.Fatalf("unified auto budget = %d, want half of RAM", got)
	}
	discrete := &typesv1.CapabilityProfile{RamTotalMb: 32768, Gpus: []*typesv1.GpuInfo{{Vendor: "nvidia", VramMb: 24576}}}
	if got := AutoBudgetMB(discrete, 80); got != 24576*80/100 {
		t.Fatalf("discrete auto budget = %d", got)
	}
	cpu := &typesv1.CapabilityProfile{RamTotalMb: 16384, Gpus: []*typesv1.GpuInfo{{Vendor: "none", Model: "cpu"}}}
	if got := AutoBudgetMB(cpu, 80); got != 8192 {
		t.Fatalf("cpu-only auto budget = %d", got)
	}
	if got := BudgetMB(4096, unified, 80); got != 4096 {
		t.Fatalf("configured budget ignored: %d", got)
	}
}

func TestProcessFootprintSelf(t *testing.T) {
	mb, err := ProcessFootprintMB(os.Getpid())
	switch runtime.GOOS {
	case "darwin", "linux":
		if err != nil {
			t.Fatal(err)
		}
		if mb < 1 {
			t.Fatalf("footprint of the test binary = %d MB, want >= 1", mb)
		}
		// Sanity: a Go test binary is nowhere near a gigabyte.
		if mb > 2048 {
			t.Fatalf("footprint = %d MB, implausible", mb)
		}
	default:
		if err == nil {
			t.Fatal("expected ErrUnsupported")
		}
	}
	if _, err := ProcessFootprintMB(-1); err == nil && runtime.GOOS != "windows" {
		t.Fatal("bogus pid measured without error")
	}
}

func TestResolveContext(t *testing.T) {
	cases := []struct{ override, model, maxCtx, want int }{
		{0, 131072, 16384, 16384}, // catalog window capped
		{0, 8192, 16384, 8192},    // small model keeps its own window
		{4096, 131072, 16384, 4096},
		{65536, 131072, 16384, 16384}, // override is capped too
		{0, 131072, 0, 131072},        // no cap
		{0, 0, 16384, 16384},          // unknown model window: pin the cap
	}
	for _, c := range cases {
		if got := ResolveContext(c.override, c.model, c.maxCtx); got != c.want {
			t.Errorf("ResolveContext(%d,%d,%d) = %d, want %d", c.override, c.model, c.maxCtx, got, c.want)
		}
	}
}
