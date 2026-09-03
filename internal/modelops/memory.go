package modelops

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/teraflock/flockd/internal/activity"
	"github.com/teraflock/flockd/internal/engine"
	"github.com/teraflock/flockd/internal/memory"
	"github.com/teraflock/flockd/internal/models"
)

// ErrOverMemory means a load does not fit the memory budget even after
// unloading every idle instance admission is allowed to touch. The assign
// service reports it to the coordinator as `cached` (the artifact stays on
// disk, loadable when demand returns); the local API returns it to the
// operator as an error.
var ErrOverMemory = errors.New("modelops: over memory budget")

// housekeepInterval is how often the idle-unload / measurement loop runs
// (a var for tests).
var housekeepInterval = 30 * time.Second

// loadInfo is what admission knows about one loaded model.
type loadInfo struct {
	Origin     string
	EstimateMB int64 // pre-load prediction (memory.EstimateMB)
	MeasuredMB int64 // last runtime footprint sample; 0 = not yet measured
}

// MemorySnapshot is the /api/v1/status memory view and the heartbeat's
// ram_used_mb source.
type MemorySnapshot struct {
	UsedMB   int64
	BudgetMB int64
	TotalMB  int64
	// Models is the per-model footprint (measured when available, else the
	// estimate) for every loaded model.
	Models map[string]int64
}

// MemoryBudgetMB is the live budget for loaded models: the configured
// budget.max_ram_mb, or the auto derivation from hardware when it is 0.
// 0 means no hardware profile and no configured budget: admission is off.
func (s *Service) MemoryBudgetMB() int64 {
	return memory.BudgetMB(s.memBudget.Load(), s.Hardware, s.Budget.MaxVRAMPercent)
}

// SetMemoryBudgetMB changes the configured budget live (0 = auto). Nothing
// is unloaded immediately; the next load and the idle loop apply it.
func (s *Service) SetMemoryBudgetMB(mb int64) { s.memBudget.Store(max(mb, 0)) }

// ConfiguredMemoryBudgetMB returns the raw configured value (0 = auto).
func (s *Service) ConfiguredMemoryBudgetMB() int64 { return s.memBudget.Load() }

// IdleUnload is the live idle-unload threshold (0 = never).
func (s *Service) IdleUnload() time.Duration { return time.Duration(s.idleUnload.Load()) }

// SetIdleUnload changes the threshold live.
func (s *Service) SetIdleUnload(d time.Duration) { s.idleUnload.Store(int64(max(d, 0))) }

// Memory reports the current accounting.
func (s *Service) Memory() MemorySnapshot {
	snap := MemorySnapshot{BudgetMB: s.MemoryBudgetMB(), Models: map[string]int64{}}
	if s.Hardware != nil {
		snap.TotalMB = int64(s.Hardware.GetRamTotalMb())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.Eng.Models() {
		mb := s.footprintLocked(m.Spec.ID)
		snap.Models[m.Spec.ID] = mb
		snap.UsedMB += mb
	}
	return snap
}

// footprintLocked is a loaded model's current charge against the budget:
// the measured footprint once one exists, else the pre-load estimate. On
// discrete GPUs the host-side measurement misses VRAM, so the estimate
// stays (TODO(nvml): read per-process VRAM use).
func (s *Service) footprintLocked(id string) int64 {
	li, ok := s.loads[id]
	if !ok {
		return 0
	}
	if li.MeasuredMB > 0 && !memory.Discrete(s.Hardware) {
		return li.MeasuredMB
	}
	return li.EstimateMB
}

func (s *Service) usedMB() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var used int64
	for _, m := range s.Eng.Models() {
		used += s.footprintLocked(m.Spec.ID)
	}
	return used
}

// admit makes room for a load of estimateMB: unloads idle instances (mesh
// placed before the operator's own, least recently used first; never the
// default model, never one with requests in flight), and fails with
// ErrOverMemory when that is not enough. Callers hold admitMu.
func (s *Service) admit(ctx context.Context, id string, estimateMB int64) error {
	budget := s.MemoryBudgetMB()
	if budget <= 0 {
		return nil // no budget known: nothing to enforce
	}
	used := s.usedMB()
	if used+estimateMB <= budget {
		return nil
	}
	def := s.Eng.DefaultModel()
	type cand struct {
		id       string
		mesh     bool
		lastUsed time.Time
	}
	var cands []cand
	s.mu.Lock()
	for _, m := range s.Eng.Models() {
		mid := m.Spec.ID
		if mid == id || mid == def {
			continue
		}
		u, ok := s.Eng.Usage(mid)
		if !ok || u.Inflight > 0 {
			continue
		}
		li := s.loads[mid]
		cands = append(cands, cand{id: mid, mesh: li != nil && li.Origin == models.OriginMesh, lastUsed: u.LastUsed})
	}
	s.mu.Unlock()
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].mesh != cands[j].mesh {
			return cands[i].mesh // mesh-placed go first
		}
		return cands[i].lastUsed.Before(cands[j].lastUsed)
	})
	for _, c := range cands {
		if used+estimateMB <= budget {
			break
		}
		s.log().Info("unloading idle model to make room", "model", c.id, "for", id,
			"need_mb", estimateMB, "used_mb", used, "budget_mb", budget)
		if err := s.unload(ctx, c.id, activity.ActorDaemon, fmt.Sprintf("memory pressure: making room for %s", id)); err != nil {
			s.log().Warn("admission unload failed", "model", c.id, "err", err)
			continue
		}
		used = s.usedMB()
	}
	if used+estimateMB > budget {
		return fmt.Errorf("%w: %s needs ~%d MB, %d of %d MB in use and nothing idle to unload", ErrOverMemory, id, estimateMB, used, budget)
	}
	return nil
}

// Measure samples every loaded runtime's footprint and replaces the
// estimates. Cheap on macOS/Linux (one syscall / one procfs read per
// process, inside Instance.Health).
func (s *Service) Measure(ctx context.Context) {
	for _, m := range s.Eng.Models() {
		hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		st, err := m.Instance.Health(hctx)
		cancel()
		if err != nil || st.MemUsedMB <= 0 {
			continue
		}
		s.mu.Lock()
		if li, ok := s.loads[m.Spec.ID]; ok {
			li.MeasuredMB = st.MemUsedMB
		}
		s.mu.Unlock()
	}
}

// IdleSince reports when a loaded model last served a request, if it is
// idle right now (no requests in flight).
func (s *Service) IdleSince(id string) (time.Time, bool) {
	u, ok := s.Eng.Usage(id)
	if !ok || u.Inflight > 0 {
		return time.Time{}, false
	}
	return u.LastUsed, true
}

// UnloadIdle unloads every non-default model idle for longer than the
// threshold. Returns the ids unloaded.
func (s *Service) UnloadIdle(ctx context.Context) []string {
	threshold := s.IdleUnload()
	if threshold <= 0 {
		return nil
	}
	def := s.Eng.DefaultModel()
	var out []string
	for _, m := range s.Eng.Models() {
		id := m.Spec.ID
		if id == def {
			continue
		}
		since, idle := s.IdleSince(id)
		if !idle || time.Since(since) < threshold {
			continue
		}
		if err := s.unload(ctx, id, activity.ActorDaemon, "idle for "+time.Since(since).Round(time.Second).String()); err != nil {
			s.log().Warn("idle unload failed", "model", id, "err", err)
			continue
		}
		out = append(out, id)
	}
	return out
}

// RunHousekeeping measures footprints and applies idle unload every
// housekeepInterval until ctx ends.
func (s *Service) RunHousekeeping(ctx context.Context) {
	t := time.NewTicker(housekeepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Measure(ctx)
			s.UnloadIdle(ctx)
		}
	}
}

var _ = engine.ErrModelNotFound
