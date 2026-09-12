package memory

// Plan is the context and slot layout one model load runs with, decided by
// PlanContext from the memory the node can actually spare (flockd#46).
//
// The order of things is the point: memory is the input and context is
// what it buys. Before this, the model's training window (capped by
// runtime.max_context) was passed as --ctx-size, llama-server split it
// across --parallel slots, and per-request context was whatever that
// division produced; the footprint was checked afterwards.
type Plan struct {
	// Slots is llama-server's --parallel: requests decoded concurrently.
	Slots int
	// CtxPerSlot is the context every request gets. Evenly split for now;
	// a per-model request from the coordinator is a later phase.
	CtxPerSlot int
	// TotalCtx is --ctx-size (Slots × CtxPerSlot), what the KV cache is
	// sized by — once, whatever the slot count (see EstimateMB).
	TotalCtx int
	// EstimateMB is the load's predicted footprint at TotalCtx.
	EstimateMB int64
	// Squeezed reports that memory, not the caps, decided the layout:
	// fewer slots than the ceiling, or a context below the floor because
	// even one slot did not fit. Logged so operators can see what their
	// budget bought.
	Squeezed bool
}

// PlanInput is what PlanContext needs to know.
type PlanInput struct {
	// BudgetMB is the admission budget and UsedMB what loaded models
	// already charge against it. BudgetMB <= 0 means no budget is known:
	// nothing limits the plan but the caps.
	BudgetMB int64
	UsedMB   int64
	// FileBytes is the model's on-disk size (weights and KV cost derive
	// from it, as in EstimateMB); MinRAMMB the catalog's floor, 0 = none.
	FileBytes int64
	MinRAMMB  int64
	// Window is the model's training context; 0 = unknown, DefaultContext.
	Window int
	// Slots is the ceiling on --parallel (budget.max_concurrent, resolved
	// by DefaultSlots when the operator left it auto). < 1 is treated as 1.
	Slots int
	// MinCtx is the per-slot floor (runtime.min_context; <= 0 =
	// DefaultMinContext): slots are given up before a request gets less.
	MinCtx int
	// MaxCtx caps per-slot context (runtime.max_context; 0 = the window).
	MaxCtx int
	// CtxPin is runtime.context_length: the operator pinning per-slot
	// context exactly (0 = plan it). Slots still adapt to memory.
	CtxPin int
}

const (
	// DefaultMinContext is the least context a request should get by
	// default: "almost nobody is ok with a tiny context" (flockd#46).
	DefaultMinContext = 8192
	// DefaultGPUSlots is the slot ceiling on accelerated nodes. Measured on
	// Metal (flockd#46): per-slot decode stops falling at ~8 slots, so
	// throughput is roughly linear in slots past that and 16 sits past the
	// knee at ~0.6 s p50 TTFB. To be re-measured on CUDA.
	DefaultGPUSlots = 16
	// DefaultCPUSlots keeps today's behaviour where nothing was measured.
	DefaultCPUSlots = 2
	// ctxGranularity rounds per-slot context down to something llama.cpp
	// batches cleanly; also the smallest context a plan will ever emit.
	ctxGranularity = 256
)

// DefaultSlots is the slot ceiling when budget.max_concurrent is 0: by
// accelerator class, since that is what the measured curve depends on.
func DefaultSlots(hw hardwareProfile) int {
	if Unified(hw) || Discrete(hw) {
		return DefaultGPUSlots
	}
	return DefaultCPUSlots
}

// FloorContext is the smallest total context a plan for in can produce:
// the per-slot floor on one slot. Admission asks for room for this much;
// the plan then grows into whatever is free once idle models have been
// unloaded for it.
func FloorContext(in PlanInput) int {
	_, floor := in.caps()
	return floor
}

// caps resolves the per-slot cap and floor from the window and the
// operator's settings. floor <= cap always.
func (in PlanInput) caps() (capCtx, floor int) {
	window := in.Window
	if window <= 0 {
		window = DefaultContext
	}
	capCtx = window
	if in.MaxCtx > 0 && in.MaxCtx < capCtx {
		capCtx = in.MaxCtx
	}
	floor = in.MinCtx
	if floor <= 0 {
		floor = DefaultMinContext
	}
	if in.CtxPin > 0 {
		// A pin is exact: it is both the cap and the floor (within the
		// window, which the runtime cannot exceed anyway).
		capCtx = min(in.CtxPin, window)
		floor = capCtx
	}
	capCtx = max(roundCtx(capCtx), ctxGranularity)
	floor = max(roundCtx(min(floor, capCtx)), ctxGranularity)
	return capCtx, floor
}

func roundCtx(n int) int { return n - n%ctxGranularity }

// PlanContext decides slots and context for one load.
//
//	spare      = BudgetMB − UsedMB − weights − overhead   (MB)
//	kv/token   = FileBytes / 65536                         (EstimateMB's constant)
//	tokens     = spare / kv per token
//	slots      = ceiling, lowered until tokens/slots ≥ floor
//	ctx/slot   = min(cap, tokens/slots)
//
// With no budget known the caps alone decide. If even one slot cannot
// reach the floor the plan is one slot at the floor and Squeezed; whether
// that loads is admission's call (EstimateMB says what it would cost).
func PlanContext(in PlanInput) Plan {
	capCtx, floor := in.caps()
	slots := max(in.Slots, 1)

	tokens := -1 // < 0: unlimited
	if in.BudgetMB > 0 && in.FileBytes > 0 {
		weights := float64(in.FileBytes) * weightsFactor / MiB
		spareMB := float64(in.BudgetMB-in.UsedMB) - weights - runtimeOverheadMB
		kvPerTok := float64(in.FileBytes) / kvBytesPerTokenDivisor
		if spareMB <= 0 {
			tokens = 0
		} else {
			tokens = int(spareMB * MiB / kvPerTok)
		}
	}

	p := Plan{}
	if tokens < 0 {
		p.Slots, p.CtxPerSlot = slots, capCtx
	} else {
		for s := slots; s >= 1; s-- {
			ctx := roundCtx(min(capCtx, tokens/s))
			if ctx >= floor {
				p.Slots, p.CtxPerSlot = s, ctx
				break
			}
		}
		if p.Slots == 0 {
			p.Slots, p.CtxPerSlot, p.Squeezed = 1, floor, true
		}
		if p.Slots < slots || p.CtxPerSlot < capCtx {
			p.Squeezed = true
		}
	}
	p.TotalCtx = p.Slots * p.CtxPerSlot
	p.EstimateMB = EstimateMB(in.FileBytes, in.MinRAMMB, p.TotalCtx)
	return p
}
