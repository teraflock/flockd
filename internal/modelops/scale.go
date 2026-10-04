package modelops

import (
	"context"
	"errors"
	"fmt"
	"math/bits"
	"sort"
	"time"

	"github.com/teraflock/flockd/internal/activity"
	"github.com/teraflock/flockd/internal/engine"
	"github.com/teraflock/flockd/internal/memory"
)

// Slot scaling (flockd#54): a chat model's slot count follows demand
// between the floor (budget.min_concurrent, auto 2) and the ceiling
// (budget.max_concurrent, auto 16 on a GPU).
//
// Loading at the ceiling made one request at a time cost the memory of
// sixteen: slots x context of KV cache, most of a 32 GB budget for a 2 GB
// model, with nothing else fitting beside it. A model now starts at the
// floor, grows when requests are actually waiting, and shrinks back when
// the demand is gone or another model needs the room.
//
// A slot count is a llama-server start flag, so a resize is a second
// runtime started with the new layout and swapped in (engine.Replace):
// requests in flight finish on the old one, requests waiting for a slot
// and everything after go to the new one, and the old one is shut down
// when it is empty. Nothing is cancelled or refused for a resize. The
// price is the memory of both layouts for the length of the drain, which
// is planned for: a grow has to fit in free memory next to the instance
// it replaces.
//
// Timings are vars so tests (and a scratch daemon) can shorten them.
var (
	// scaleInterval is how often demand is sampled.
	scaleInterval = time.Second
	// growAfter: demand at or above the slot count for this long grows.
	growAfter = 3 * time.Second
	// shrinkAfter: peak demand fitting in half the slots for this long
	// shrinks.
	shrinkAfter = 5 * time.Minute
	// resizeCooldown is the least time between two resizes of a model,
	// and the back-off after an attempt that did not fit or failed.
	resizeCooldown = time.Minute
	// drainTimeout bounds how long a replaced instance may keep finishing
	// its requests before it is shut down regardless.
	drainTimeout = 30 * time.Minute
)

// SetScaleTimings overrides the scaling windows (tests, scratch runs);
// zero values keep the current setting.
func SetScaleTimings(grow, shrink, cooldown time.Duration) {
	if grow > 0 {
		growAfter = grow
	}
	if shrink > 0 {
		shrinkAfter = shrink
	}
	if cooldown > 0 {
		resizeCooldown = cooldown
	}
}

// errNoRoom: the larger layout does not fit in free memory.
var errNoRoom = errors.New("modelops: no room to resize")

// pow2ceil is the smallest power of two >= n (n >= 1).
func pow2ceil(n int) int {
	if n <= 1 {
		return 1
	}
	return 1 << bits.Len(uint(n-1))
}

// growTarget is the slot count to grow to from cur under a peak demand:
// the smallest power of two that holds the peak, at least double, never
// above the ceiling. Equal to cur when there is nowhere to go.
func growTarget(cur, peak, ceiling int) int {
	return min(max(pow2ceil(peak), cur*2), ceiling)
}

// shrinkTarget is the slot count to shrink to when the peak demand of the
// shrink window was peak: the smallest power of two that holds it, never
// below the floor. Shrinking happens only when that is at most half of
// cur (hysteresis: a model serving 5 of 8 slots stays at 8).
func shrinkTarget(cur, peak, floor int) int {
	t := max(pow2ceil(max(peak, 1)), floor)
	if t*2 > cur {
		return cur
	}
	return t
}

// RunSlotScaling follows demand until ctx ends.
func (s *Service) RunSlotScaling(ctx context.Context) {
	t := time.NewTicker(scaleInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.ScaleTick(ctx, time.Now())
		}
	}
}

// ScaleTick samples every scalable model's demand once and resizes the
// ones whose windows say so. Exported for tests, which drive the clock.
func (s *Service) ScaleTick(ctx context.Context, now time.Time) {
	lim := s.PlanLimits()
	floor, ceiling := lim.floor(s.Hardware), lim.slots(s.Hardware)
	type job struct {
		id     string
		target int
		demand int
	}
	var jobs []job
	for _, m := range s.Eng.Models() {
		id := m.Spec.ID
		_, peak, ok := s.Eng.Demand(id)
		if !ok {
			continue
		}
		s.mu.Lock()
		li, ok := s.loads[id]
		if !ok || !li.Scalable || li.Limits != lim || s.loading[id] {
			// Not ours to scale, or planned under other limits: the
			// settings reload (ReloadStale) deals with those.
			s.mu.Unlock()
			continue
		}
		cur := li.Slots
		// Saturation: demand has reached the slot count.
		if peak >= cur {
			if li.SatSince.IsZero() {
				li.SatSince, li.SatPeak = now, 0
			}
			li.SatPeak = max(li.SatPeak, peak)
		} else {
			li.SatSince, li.SatPeak = time.Time{}, 0
		}
		// Slack: the peak fits in half the slots.
		if peak*2 <= cur {
			if li.LowSince.IsZero() {
				li.LowSince, li.LowPeak = now, 0
			}
			li.LowPeak = max(li.LowPeak, peak)
		} else {
			li.LowSince, li.LowPeak = time.Time{}, 0
		}
		cooled := now.Sub(li.Resized) >= resizeCooldown && now.Sub(li.Attempted) >= resizeCooldown
		var j *job
		switch {
		case !cooled:
		case !li.SatSince.IsZero() && now.Sub(li.SatSince) >= growAfter:
			if t := growTarget(cur, li.SatPeak, ceiling); t > cur {
				j = &job{id: id, target: t, demand: li.SatPeak}
			}
		case !li.LowSince.IsZero() && now.Sub(li.LowSince) >= shrinkAfter:
			if t := shrinkTarget(cur, li.LowPeak, floor); t < cur {
				j = &job{id: id, target: t, demand: li.LowPeak}
			}
		}
		if j != nil {
			li.Attempted = now
		}
		s.mu.Unlock()
		if j != nil {
			jobs = append(jobs, *j)
		}
	}
	sort.Slice(jobs, func(i, k int) bool { return jobs[i].id < jobs[k].id })
	for _, j := range jobs {
		from, to, err := s.Resize(ctx, j.id, j.target)
		switch {
		case errors.Is(err, errNoRoom):
			s.log().Info("slots not grown: no free memory for a larger layout", "model", j.id,
				"slots", from, "wanted", j.target, "demand", j.demand)
		case err != nil:
			s.log().Warn("slot resize failed; the model keeps its layout", "model", j.id, "err", err)
		default:
			s.log().Info("slots resized to follow demand", "model", j.id, "from", from, "to", to, "demand", j.demand)
			s.mu.Lock()
			if li, ok := s.loads[j.id]; ok {
				li.Resized = now // the tick's clock, which the cooldown is measured on
			}
			s.mu.Unlock()
		}
	}
}

// Resize restarts a loaded model with another slot count, without
// unloading it: a new runtime is started with the layout planned for
// target slots in the memory that is free right now, swapped in, and the
// old one shut down once its requests have finished. It returns the slot
// counts before and after. errNoRoom when a grow does not fit (the plan
// yields no more slots than the model has).
func (s *Service) Resize(ctx context.Context, id string, target int) (from, to int, err error) {
	s.admitMu.Lock()
	defer s.admitMu.Unlock()

	s.mu.Lock()
	li, ok := s.loads[id]
	if !ok {
		s.mu.Unlock()
		return 0, 0, fmt.Errorf("%w: %q not loaded", engine.ErrModelNotFound, id)
	}
	from = li.Slots
	in, spec, oldEstimate := li.Plan, li.Spec, s.footprintLocked(id)
	s.mu.Unlock()
	if target == from {
		return from, from, nil
	}

	// Planned in the memory that is free with the old instance still
	// there: both run until the old one has drained.
	in.Slots = target
	in.BudgetMB, in.UsedMB = s.MemoryBudgetMB(), s.usedMB()
	plan := memory.PlanContext(in)
	if in.BudgetMB > 0 && plan.EstimateMB > in.BudgetMB-in.UsedMB {
		return from, from, errNoRoom
	}
	if target > from && plan.Slots <= from {
		return from, from, errNoRoom
	}
	res := s.Budget
	res.MaxConcurrent = s.SlotCeiling()
	res.Slots, res.ContextTokens = plan.Slots, plan.TotalCtx
	s.log().Info("context plan", "model", id, "slots", plan.Slots, "ctx_per_slot", plan.CtxPerSlot,
		"ctx_total", plan.TotalCtx, "estimate_mb", plan.EstimateMB, "used_mb", in.UsedMB,
		"budget_mb", in.BudgetMB, "squeezed", plan.Squeezed, "resize_from", from)

	inst, err := s.Loader.Load(ctx, spec, res)
	if err != nil {
		return from, from, err
	}
	old := s.Eng.Replace(spec, inst, plan.Slots)
	if old == nil {
		// Unloaded while the new runtime was starting.
		_ = inst.Shutdown(ctx)
		return from, from, fmt.Errorf("%w: %q was unloaded during the resize", engine.ErrModelNotFound, id)
	}
	now := time.Now()
	s.mu.Lock()
	if li, ok := s.loads[id]; ok {
		li.EstimateMB, li.MeasuredMB, li.VRAMSeq = plan.EstimateMB, 0, s.vramSampleSeq
		li.Slots, li.CtxPerSlot = plan.Slots, plan.CtxPerSlot
		li.Resized, li.SatSince, li.LowSince = now, time.Time{}, time.Time{}
	}
	s.drainingMB += oldEstimate
	s.mu.Unlock()
	go s.drain(id, old, oldEstimate)

	if s.OnLoaded != nil {
		s.OnLoaded(inst)
	}
	s.Events.Publish("models_changed", map[string]string{"model": id, "change": "resized"})
	s.Activity.Record(activity.KindResized, activity.ActorDaemon, id,
		fmt.Sprintf("%s: %d → %d slots (%d tokens each, ~%d MB)", id, from, plan.Slots, plan.CtxPerSlot, plan.EstimateMB),
		"following demand")
	return from, plan.Slots, nil
}

// drain shuts a replaced instance down once the requests it was serving
// have finished, and gives its memory back to the accounting.
func (s *Service) drain(id string, old *engine.ModelEntry, estimateMB int64) {
	deadline := time.Now().Add(drainTimeout)
	for old.Draining() > 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if n := old.Draining(); n > 0 {
		s.log().Warn("replaced runtime still busy after the drain timeout; shutting it down", "model", id, "inflight", n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := old.Instance.Shutdown(ctx); err != nil {
		s.log().Warn("shutdown of the replaced runtime", "model", id, "err", err)
	}
	s.mu.Lock()
	s.drainingMB -= estimateMB
	s.mu.Unlock()
}

// shrinkForRoom is admission's first resort when a load needs memory:
// grown models that are idle go back to the floor, in place (they are
// idle, so the restart interrupts nothing). It returns true when anything
// was shrunk. Callers hold admitMu.
func (s *Service) shrinkForRoom(ctx context.Context, forID string) bool {
	lim := s.PlanLimits()
	floor := lim.floor(s.Hardware)
	type cand struct {
		id     string
		origin string
	}
	var cands []cand
	s.mu.Lock()
	for id, li := range s.loads {
		if id != forID && li.Scalable && li.Slots > floor {
			cands = append(cands, cand{id, li.Origin})
		}
	}
	s.mu.Unlock()
	sort.Slice(cands, func(i, j int) bool { return cands[i].id < cands[j].id })
	def := s.Eng.DefaultModel()
	shrunk := false
	for _, c := range cands {
		s.mu.Lock()
		li := s.loads[c.id]
		if li == nil {
			s.mu.Unlock()
			continue
		}
		in, spec, from := li.Plan, li.Spec, li.Slots
		s.mu.Unlock()
		err := s.unload(ctx, c.id, unloadOpts{actor: activity.ActorDaemon, idleOnly: true, reloading: true,
			reason: "memory pressure: shrinking to make room for " + forID})
		if err != nil {
			continue // busy: its slots are in use, leave it
		}
		in.Slots = floor
		in.BudgetMB, in.UsedMB = s.MemoryBudgetMB(), s.usedMB()
		plan := memory.PlanContext(in)
		res := s.Budget
		res.MaxConcurrent = lim.slots(s.Hardware)
		res.Slots, res.ContextTokens = plan.Slots, plan.TotalCtx
		inst, err := s.Loader.Load(ctx, spec, res)
		if err != nil {
			s.log().Warn("shrink for room: restart failed; model left unloaded", "model", c.id, "err", err)
			if s.OnUnloaded != nil {
				s.OnUnloaded(c.id)
			}
			shrunk = true // its memory is free either way
			continue
		}
		now := time.Now()
		s.mu.Lock()
		s.loads[c.id] = &loadInfo{Origin: c.origin, EstimateMB: plan.EstimateMB, LoadedAt: now, VRAMSeq: s.vramSampleSeq,
			Limits: lim, Slots: plan.Slots, CtxPerSlot: plan.CtxPerSlot, Spec: spec, Plan: li.Plan, Scalable: true, Resized: now}
		s.mu.Unlock()
		s.Eng.RegisterSlots(spec, inst, plan.Slots)
		if c.id == def {
			_ = s.Eng.SetDefault(def)
		}
		s.log().Info("slots shrunk to make room", "model", c.id, "from", from, "to", plan.Slots, "for", forID)
		s.Events.Publish("models_changed", map[string]string{"model": c.id, "change": "resized"})
		s.Activity.Record(activity.KindResized, activity.ActorDaemon, c.id,
			fmt.Sprintf("%s: %d → %d slots to make room for %s", c.id, from, plan.Slots, forID), "memory pressure")
		shrunk = true
	}
	return shrunk
}
