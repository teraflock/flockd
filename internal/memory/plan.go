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
	// KVBytesPerToken is the model's KV-cache cost per context token from
	// its GGUF header (gguf.Meta.KVBytesPerToken); 0 = unknown, and the
	// file-size heuristic in EstimateMB stands in. The heuristic is 2–4x low
	// for small GQA models, which is exactly where over-planning hurts.
	KVBytesPerToken int64
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
	// EncoderMBPerToken > 0 marks a whole-prompt-batch encoder (a
	// decision model on a BERT-family architecture: Laya, Julia). It has
	// no KV cache: what grows is the compute buffer for one batch, by
	// this many MB per token of the longest prompt a slot admits, once,
	// whatever the slot count. See DecisionPlanInput.
	EncoderMBPerToken float64
}

// Decision model planning (flockd#53). A decision request is one forward
// pass over a short prompt (measured on Metal, llama.cpp b11382: Laya and
// Julia-1 10-30 ms, Kev-4B 100-240 ms), and a decision model usually
// shares the node with a chat model, so its plan is modest and fixed
// instead of growing into whatever the budget has free.
const (
	// DecisionEncoderSlots: slots cost an encoder no memory (measured:
	// Julia-1 with 1, 4 or 11 slots has the same footprint) and its
	// requests are evaluated one after another anyway.
	DecisionEncoderSlots = 4
	// DecisionEncoderContext caps an encoder's per-slot context, which is
	// also its batch size and the whole of its memory cost: Julia-1 holds
	// ~1 MB per token of the longest prompt it has seen (8 GB after one
	// 7,500-token prompt against a 168 MB file). 2,048 tokens is a state
	// of some 1,500 words; runtime.context_length raises it.
	DecisionEncoderContext = 2048
	// DecisionCausalSlots and DecisionCausalContext are the plan for a
	// causal decision model (Kev, Clef): each slot costs its context in
	// KV cache like a chat model's. 8,192 is the length Kev's accuracy is
	// validated to; runtime.context_length raises it.
	DecisionCausalSlots   = 2
	DecisionCausalContext = 8192
	// decisionMinContext is the floor a squeezed decision plan stops at.
	decisionMinContext = 1024
	// DefaultEncoderMBPerToken is the batch cost assumed for an encoder
	// family nobody measured: Julia-1's, the larger of the two known.
	DefaultEncoderMBPerToken = 1.1
)

// encoderMBPerToken is the measured compute-buffer growth per prompt
// token, by catalog family (llama.cpp b11382, Metal, Q8_0, footprint after
// a prompt of N tokens minus the idle footprint):
//
//	julia: 414 tok +350 MB, 1,814 +1,750, 3,714 +3,745, 7,514 +7,800
//	laya:  428 tok  +90 MB, 1,828   +395, 3,728   +855, 7,528 +1,940
var encoderMBPerToken = map[string]float64{
	"julia": 1.1,
	"laya":  0.3,
}

// EncoderMBPerToken is the batch cost per token for an encoder family.
func EncoderMBPerToken(family string) float64 {
	if v, ok := encoderMBPerToken[family]; ok {
		return v
	}
	return DefaultEncoderMBPerToken
}

// DecisionPlanInput turns in into the plan input for a decision model:
// few slots and a per-slot context sized for decision prompts, capped by
// whatever the operator already capped (runtime.max_context). encoder
// says the model is a whole-prompt-batch encoder (GGUF architecture
// *bert*), which is then also costed as one: no KV cache, a batch buffer.
// A runtime.context_length pin still wins (caps treats it as exact).
func DecisionPlanInput(in PlanInput, encoder bool, family string) PlanInput {
	slots, ctx := DecisionCausalSlots, DecisionCausalContext
	if encoder {
		slots, ctx = DecisionEncoderSlots, DecisionEncoderContext
		in.EncoderMBPerToken = EncoderMBPerToken(family)
	}
	in.Slots = min(max(in.Slots, 1), slots)
	if in.MaxCtx <= 0 || in.MaxCtx > ctx {
		in.MaxCtx = ctx
	}
	in.MinCtx = decisionMinContext
	return in
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

// EstimateAt is the load's footprint at a total context of ctx tokens,
// with the header's KV cost when in carries one.
//
// For an encoder (EncoderMBPerToken > 0) ctx is the per-slot context: its
// footprint does not depend on the slot count.
func (in PlanInput) EstimateAt(ctx int) int64 {
	if in.EncoderMBPerToken > 0 {
		return in.encoderEstimate(ctx)
	}
	return estimateMB(in.FileBytes, in.MinRAMMB, ctx, in.KVBytesPerToken)
}

// encoderEstimate is an encoder's footprint with a batch of ctxPerSlot
// tokens: weights and process overhead as for any model, plus the batch
// buffer at its largest.
func (in PlanInput) encoderEstimate(ctxPerSlot int) int64 {
	weights := float64(in.FileBytes) * weightsFactor / MiB
	est := int64(weights+in.EncoderMBPerToken*float64(ctxPerSlot)) + runtimeOverheadMB
	return max(est, in.MinRAMMB)
}

// planEncoder plans a whole-prompt-batch encoder: the slot ceiling as
// given (slots are free), and the largest per-slot context up to the cap
// whose batch buffer fits what the budget has left.
func planEncoder(in PlanInput) Plan {
	capCtx, floor := in.caps()
	p := Plan{Slots: max(in.Slots, 1), CtxPerSlot: capCtx}
	if in.BudgetMB > 0 {
		spare := in.BudgetMB - in.UsedMB
		for p.CtxPerSlot > floor && in.encoderEstimate(p.CtxPerSlot) > spare {
			p.CtxPerSlot -= ctxGranularity
			p.Squeezed = true
		}
		if in.encoderEstimate(p.CtxPerSlot) > spare {
			p.Squeezed = true // does not fit even at the floor: admission's call
		}
	}
	p.TotalCtx = p.Slots * p.CtxPerSlot
	p.EstimateMB = in.encoderEstimate(p.CtxPerSlot)
	return p
}

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
	if in.EncoderMBPerToken > 0 {
		return planEncoder(in)
	}
	capCtx, floor := in.caps()
	slots := max(in.Slots, 1)

	tokens := -1 // < 0: unlimited
	if in.BudgetMB > 0 && in.FileBytes > 0 {
		weights := float64(in.FileBytes) * weightsFactor / MiB
		spareMB := float64(in.BudgetMB-in.UsedMB) - weights - runtimeOverheadMB
		kvPerTok := kvPerToken(in.FileBytes, in.KVBytesPerToken)
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
	p.EstimateMB = in.EstimateAt(p.TotalCtx)
	return p
}
