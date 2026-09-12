package memory

import (
	"testing"

	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
)

const (
	gb        = 1024 * MiB
	file3B    = int64(2_019_377_696) // llama-3.2-3b Q4_K_M, the laptop's copy
	file8B    = int64(49 * gb / 10)
	file24B   = int64(14 * gb)
	window3B  = 131072
	window8B  = 131072
	gpuSlots  = DefaultGPUSlots
	defaultMx = 16384 // config default runtime.max_context
)

func TestPlanNoBudgetUsesTheCaps(t *testing.T) {
	p := PlanContext(PlanInput{FileBytes: file3B, Window: window3B, Slots: gpuSlots, MaxCtx: defaultMx})
	if p.Slots != gpuSlots || p.CtxPerSlot != defaultMx || p.TotalCtx != gpuSlots*defaultMx || p.Squeezed {
		t.Fatalf("no budget: %+v, want %d slots × %d", p, gpuSlots, defaultMx)
	}
	if p.EstimateMB != EstimateMB(file3B, 0, p.TotalCtx) {
		t.Fatal("estimate is not the footprint at the planned total context")
	}
	// The window caps when max_context is 0.
	if p := PlanContext(PlanInput{FileBytes: file3B, Window: 8192, Slots: 4}); p.CtxPerSlot != 8192 {
		t.Fatalf("window not honoured as the cap: %+v", p)
	}
	// Unknown window: DefaultContext.
	if p := PlanContext(PlanInput{FileBytes: file3B, Slots: 1}); p.CtxPerSlot != DefaultContext {
		t.Fatalf("unknown window: %+v, want %d", p, DefaultContext)
	}
}

// The laptop: 64 GB, 32 GB budget. The 3B fits 16 slots at the cap with
// room to spare; the 24B cannot give 16 slots 8k each and drops slots
// before context.
func TestPlanSpendsSpareMemoryOnContextThenSlots(t *testing.T) {
	budget := int64(32 * 1024)
	p := PlanContext(PlanInput{BudgetMB: budget, FileBytes: file3B, Window: window3B, Slots: gpuSlots, MaxCtx: defaultMx})
	if p.Slots != gpuSlots || p.CtxPerSlot != defaultMx || p.Squeezed {
		t.Fatalf("3B on 32 GB: %+v, want the full %d × %d", p, gpuSlots, defaultMx)
	}
	if p.EstimateMB >= budget {
		t.Fatalf("3B plan does not fit its own budget: %d MB", p.EstimateMB)
	}

	// 8B at 75 KB/token: 16 × 16k ≈ 19 GB of KV on top of 5.6 GB weights —
	// fits, barely, so still the full layout.
	p = PlanContext(PlanInput{BudgetMB: budget, FileBytes: file8B, Window: window8B, Slots: gpuSlots, MaxCtx: defaultMx})
	if p.EstimateMB > budget {
		t.Fatalf("8B plan overshoots the budget: %+v", p)
	}
	if p.Slots < 8 {
		t.Fatalf("8B on 32 GB gave up too many slots: %+v", p)
	}

	// 24B: 16 GB of weights leaves ~15 GB; at ~214 KB/token that is ~72k
	// tokens — 8 slots at 8k, not 16.
	p = PlanContext(PlanInput{BudgetMB: budget, FileBytes: file24B, Window: 32768, Slots: gpuSlots, MaxCtx: defaultMx})
	if !p.Squeezed || p.Slots >= gpuSlots || p.CtxPerSlot < DefaultMinContext {
		t.Fatalf("24B on 32 GB: %+v, want fewer slots and ≥ %d per slot", p, DefaultMinContext)
	}
	if p.EstimateMB > budget {
		t.Fatalf("squeezed plan still overshoots: %+v", p)
	}
	// Slots go before context: every slot keeps the floor.
	if p.CtxPerSlot < DefaultMinContext {
		t.Fatalf("floor violated: %+v", p)
	}
}

func TestPlanUsedMemoryCountsAgainstIt(t *testing.T) {
	free := PlanContext(PlanInput{BudgetMB: 16 * 1024, FileBytes: file8B, Window: window8B, Slots: gpuSlots, MaxCtx: defaultMx})
	busy := PlanContext(PlanInput{BudgetMB: 16 * 1024, UsedMB: 6 * 1024, FileBytes: file8B, Window: window8B, Slots: gpuSlots, MaxCtx: defaultMx})
	if busy.TotalCtx >= free.TotalCtx {
		t.Fatalf("memory already in use did not shrink the plan: free=%+v busy=%+v", free, busy)
	}
	if busy.EstimateMB > 16*1024-6*1024 {
		t.Fatalf("busy plan ignores what is already charged: %+v", busy)
	}
}

func TestPlanBelowTheFloorIsOneSlotAtTheFloor(t *testing.T) {
	// 8 GB budget, 5.6 GB of weights: ~2 GB spare ≈ 27k tokens at 75 KB —
	// three slots at 8k. Push the floor to 32k and nothing fits: one slot
	// at the floor, squeezed, and the estimate says what it would cost.
	p := PlanContext(PlanInput{BudgetMB: 8 * 1024, FileBytes: file8B, Window: window8B, Slots: gpuSlots, MinCtx: 32768, MaxCtx: 32768})
	if p.Slots != 1 || p.CtxPerSlot != 32768 || !p.Squeezed {
		t.Fatalf("below the floor: %+v, want 1 × 32768 squeezed", p)
	}
	if p.EstimateMB <= 8*1024 {
		t.Fatalf("estimate should exceed the budget so admission can refuse it: %d", p.EstimateMB)
	}
	// Nothing spare at all (used > budget): same shape, never a panic or
	// a zero context.
	p = PlanContext(PlanInput{BudgetMB: 4 * 1024, UsedMB: 5 * 1024, FileBytes: file3B, Window: window3B, Slots: gpuSlots})
	if p.Slots != 1 || p.CtxPerSlot != DefaultMinContext || p.TotalCtx <= 0 {
		t.Fatalf("over budget before the load: %+v", p)
	}
}

func TestPlanOperatorPinAndCaps(t *testing.T) {
	// context_length pins per-slot context exactly; slots still adapt.
	p := PlanContext(PlanInput{BudgetMB: 32 * 1024, FileBytes: file3B, Window: window3B, Slots: gpuSlots, CtxPin: 4096})
	if p.CtxPerSlot != 4096 || p.Slots != gpuSlots {
		t.Fatalf("pin: %+v, want %d × 4096", p, gpuSlots)
	}
	// A pin above the window is the window.
	if p := PlanContext(PlanInput{FileBytes: file3B, Window: 8192, Slots: 2, CtxPin: 1 << 20}); p.CtxPerSlot != 8192 {
		t.Fatalf("pin above the window: %+v", p)
	}
	// max_context caps, min_context floors, and the floor never exceeds the cap.
	p = PlanContext(PlanInput{FileBytes: file3B, Window: window3B, Slots: 2, MinCtx: 65536, MaxCtx: 4096})
	if p.CtxPerSlot != 4096 {
		t.Fatalf("floor above cap: %+v, want the cap", p)
	}
	if got := FloorContext(PlanInput{Window: window3B, MinCtx: 65536, MaxCtx: 4096}); got != 4096 {
		t.Fatalf("FloorContext = %d, want 4096", got)
	}
	if got := FloorContext(PlanInput{Window: window3B}); got != DefaultMinContext {
		t.Fatalf("FloorContext default = %d, want %d", got, DefaultMinContext)
	}
	// Context is a multiple of the granularity and never zero.
	p = PlanContext(PlanInput{FileBytes: file3B, Window: 1000, Slots: 1})
	if p.CtxPerSlot != 768 {
		t.Fatalf("granularity: %+v, want 768", p)
	}
	if p := PlanContext(PlanInput{FileBytes: file3B, Window: 10, Slots: 1}); p.CtxPerSlot != ctxGranularity {
		t.Fatalf("tiny window: %+v, want %d", p, ctxGranularity)
	}
	// Slots < 1 is 1.
	if p := PlanContext(PlanInput{FileBytes: file3B, Window: 8192}); p.Slots != 1 {
		t.Fatalf("zero slots: %+v", p)
	}
}

func TestDefaultSlotsByAcceleratorClass(t *testing.T) {
	unified := &typesv1.CapabilityProfile{RamTotalMb: 65536, Gpus: []*typesv1.GpuInfo{{Vendor: "apple", VramMb: 65536, UnifiedMemory: true}}}
	discrete := &typesv1.CapabilityProfile{RamTotalMb: 32768, Gpus: []*typesv1.GpuInfo{{Vendor: "nvidia", VramMb: 24576}}}
	cpu := &typesv1.CapabilityProfile{RamTotalMb: 32768, Gpus: []*typesv1.GpuInfo{{Vendor: "none"}}}
	for _, c := range []struct {
		name string
		hw   *typesv1.CapabilityProfile
		want int
	}{
		{"unified", unified, DefaultGPUSlots},
		{"discrete", discrete, DefaultGPUSlots},
		{"cpu", cpu, DefaultCPUSlots},
		{"nil", nil, DefaultCPUSlots},
	} {
		if got := DefaultSlots(c.hw); got != c.want {
			t.Errorf("DefaultSlots(%s) = %d, want %d", c.name, got, c.want)
		}
	}
}
