// Package modelops is the daemon's on-demand model operations service:
// catalog browsing, operator-triggered downloads (with cancel), and runtime
// load/unload/default switching. It is the write-half the local API lacked —
// before it, models moved only at daemon startup or via the coordinator's
// ModelAssignment stub.
package modelops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"

	"github.com/teraflock/flockd/internal/activity"
	"github.com/teraflock/flockd/internal/engine"
	"github.com/teraflock/flockd/internal/events"
	"github.com/teraflock/flockd/internal/gguf"
	"github.com/teraflock/flockd/internal/memory"
	"github.com/teraflock/flockd/internal/models"
	rt "github.com/teraflock/flockd/internal/runtime"
)

// ErrUnknownModel is returned when an id is in neither the catalog nor the
// local cache.
var ErrUnknownModel = errors.New("modelops: model not in catalog")

// ErrDownloadRunning is returned when a download for the id is in flight.
var ErrDownloadRunning = errors.New("modelops: download already running")

// catalogTTL is how long a fetched catalog is trusted before the next
// read refetches it, and how often RunCatalogRefresh does so unasked: a
// newly promoted catalog reaches a running daemon within this long without
// a restart. The refetch is conditional (ETag / Last-Modified), so an
// unchanged catalog costs a 304.
const catalogTTL = 5 * time.Minute

// catalogMissRefetch rate-limits the refetch a lookup miss triggers (see
// Lookup): at most one per this long, however many unknown ids are asked
// for. A var so tests can shorten it.
var catalogMissRefetch = 30 * time.Second

// Loader loads a model artifact into a serving runtime instance
// (*llamacpp.Adapter in production; fakes in tests).
type Loader interface {
	Load(ctx context.Context, m rt.ModelSpec, res rt.ResourceBudget) (rt.Instance, error)
}

// Service wires catalog + cache + runtime together. Nil Service (mock
// runtime) means the local API reports these operations unsupported.
type Service struct {
	Mgr    *models.Manager
	Eng    *engine.Engine
	Loader Loader
	Budget rt.ResourceBudget
	Log    *slog.Logger

	// Catalog sources, mirroring config (path wins).
	ManifestPath string
	ManifestURL  string

	// OnLoaded is called after a model becomes servable (used to stamp the
	// runtime build id into the capability profile). May be nil.
	OnLoaded func(inst rt.Instance)

	// Events receives models_changed on download-complete, load, unload and
	// default switches. May be nil.
	Events *events.Hub
	// Activity receives loaded/unloaded rows. May be nil.
	Activity *activity.Ring

	// Hardware sizes the auto memory budget and tells unified from
	// discrete memory. Nil disables admission unless a budget is
	// configured explicitly (SetMemoryBudgetMB).
	Hardware *typesv1.CapabilityProfile
	// MaxContext mirrors runtime.max_context: the per-slot context cap
	// (0 = the model's window).
	MaxContext int
	// MinContext mirrors runtime.min_context: the per-slot floor the
	// planner gives up slots to protect (0 = memory.DefaultMinContext).
	MinContext int
	// ContextLength is the operator's runtime.context_length pin on
	// per-slot context (0 = planned from the budget).
	ContextLength int
	// MinConcurrent mirrors budget.min_concurrent: the slot floor chat
	// models start at (0 = auto; see PlanLimits).
	MinConcurrent int
	// OnUnloaded is called after any unload (operator, idle, memory
	// pressure) with the model id; the assign service reports `cached` to
	// the coordinator from it. May be nil.
	OnUnloaded func(id string)
	// VRAM measures used VRAM on discrete GPUs (nvidia-smi). Nil = built
	// from Hardware on first use; tests inject one.
	VRAM *memory.VRAMSampler

	memBudget  atomic.Int64 // configured budget.max_ram_mb; 0 = auto
	idleUnload atomic.Int64 // nanoseconds; 0 = never

	// admitMu serialises admission + runtime load so two loads cannot both
	// pass the same headroom check. Downloads happen outside it.
	admitMu sync.Mutex

	mu        sync.Mutex
	catalog   *models.Catalog
	fetchedAt time.Time
	// catalogStamp is the HTTP validators of catalog (conditional refetch);
	// catalogTried is the last fetch attempt, successful or not.
	catalogStamp models.CatalogStamp
	catalogTried time.Time
	downloads    map[string]context.CancelFunc
	loading      map[string]bool
	// drainingMB is the memory of instances replaced by a resize that are
	// still finishing their requests (scale.go).
	drainingMB int64
	// plan is the live plan limits once SetPlanLimits has been called
	// (planSet); before that the exported fields above are read.
	plan    PlanLimits
	planSet bool
	// starting is the models whose runtime is starting right now (the
	// Loader.Load call), and since when.
	starting map[string]time.Time
	loads    map[string]*loadInfo
	// Last VRAM sample (discrete GPUs); zero time = never sampled.
	vramUsedMB    int64
	vramSampledAt time.Time
	vramSampleSeq uint64 // incremented per card read; see loadInfo.VRAMSeq
}

// Catalog returns the model catalog, cached for catalogTTL. refresh forces
// a refetch. A failed refetch keeps the last good copy.
func (s *Service) Catalog(ctx context.Context, refresh bool) (*models.Catalog, error) {
	s.mu.Lock()
	if s.catalog != nil && !refresh && time.Since(s.fetchedAt) < catalogTTL {
		c := s.catalog
		s.mu.Unlock()
		return c, nil
	}
	stamp := s.catalogStamp
	if s.catalog == nil {
		stamp = models.CatalogStamp{} // nothing to keep on a 304
	}
	s.catalogTried = time.Now()
	s.mu.Unlock()

	c, newStamp, err := models.FetchCatalog(ctx, s.ManifestPath, s.ManifestURL, nil, stamp)
	if errors.Is(err, models.ErrCatalogNotModified) {
		s.mu.Lock()
		s.fetchedAt = time.Now()
		c = s.catalog
		s.mu.Unlock()
		return c, nil
	}
	if err != nil {
		// A stale catalog beats no catalog: models don't churn hourly.
		s.mu.Lock()
		stale := s.catalog
		s.mu.Unlock()
		if stale != nil {
			s.log().Warn("catalog refresh failed; serving cached copy", "err", err)
			return stale, nil
		}
		return nil, err
	}
	s.mu.Lock()
	prev := s.catalog
	s.catalog = c
	s.catalogStamp = newStamp
	s.fetchedAt = time.Now()
	s.mu.Unlock()
	if s.Mgr != nil {
		s.Mgr.Reconcile(c)
	}
	if prev != nil && !sameCatalog(prev, c) {
		s.log().Info("catalog updated", "models", len(c.Models), "was", len(prev.Models))
		s.Events.Publish("models_changed", map[string]string{"change": "catalog"})
	}
	return c, nil
}

// sameCatalog reports whether two catalogs list the same artifacts.
func sameCatalog(a, b *models.Catalog) bool {
	if len(a.Models) != len(b.Models) {
		return false
	}
	for i := range a.Models {
		if a.Models[i].ID != b.Models[i].ID || a.Models[i].SHA256 != b.Models[i].SHA256 {
			return false
		}
	}
	return true
}

// Lookup finds a catalog entry by id. A miss triggers one refetch before
// the answer is "not in catalog" — a model promoted since the last fetch
// is found without waiting for the TTL or a restart — rate-limited to one
// refetch per catalogMissRefetch so unknown ids cannot hammer the host.
func (s *Service) Lookup(ctx context.Context, id string) (models.CatalogModel, bool, error) {
	cat, err := s.Catalog(ctx, false)
	if err != nil {
		return models.CatalogModel{}, false, err
	}
	if m, ok := cat.Find(id); ok {
		return m, true, nil
	}
	if cat, ok := s.refetchOnMiss(ctx); ok {
		m, found := cat.Find(id)
		return m, found, nil
	}
	return models.CatalogModel{}, false, nil
}

// refetchOnMiss refetches the catalog after a lookup miss unless one was
// attempted within catalogMissRefetch; ok is false when it did not.
func (s *Service) refetchOnMiss(ctx context.Context) (*models.Catalog, bool) {
	s.mu.Lock()
	recent := time.Since(s.catalogTried) < catalogMissRefetch
	s.mu.Unlock()
	if recent {
		return nil, false
	}
	cat, err := s.Catalog(ctx, true)
	if err != nil {
		return nil, false
	}
	return cat, true
}

// RunCatalogRefresh refetches the catalog every interval (<= 0:
// catalogTTL) until ctx ends, so a daemon that nobody asks about models
// still learns of a promoted catalog. Failures keep the last good copy.
func (s *Service) RunCatalogRefresh(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = catalogTTL
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := s.Catalog(ctx, true); err != nil {
				s.log().Warn("catalog refresh failed", "err", err)
			}
		}
	}
}

// StartDownload begins fetching a catalog model in the background. It
// returns (false, nil) if the model is already cached and ready, and
// ErrDownloadRunning if a download is already in flight.
func (s *Service) StartDownload(ctx context.Context, id string) (bool, error) {
	if err := models.ValidateID(id); err != nil {
		return false, err
	}
	for _, i := range s.Mgr.List() {
		if i.ID == id && i.State == "ready" {
			return false, nil
		}
	}
	entry, ok, err := s.Lookup(ctx, id)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, fmt.Errorf("%w: %q", ErrUnknownModel, id)
	}

	s.mu.Lock()
	if s.downloads == nil {
		s.downloads = map[string]context.CancelFunc{}
	}
	if _, running := s.downloads[id]; running {
		s.mu.Unlock()
		return false, ErrDownloadRunning
	}
	// Detached from the request context: closing the browser tab that
	// clicked "download" must not abort a 20 GB transfer.
	dlCtx, cancel := context.WithCancel(context.Background())
	s.downloads[id] = cancel
	s.mu.Unlock()

	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.downloads, id)
			s.mu.Unlock()
			cancel()
		}()
		s.log().Info("download started", "model", id, "size_mb", entry.SizeBytes/1024/1024)
		if _, err := s.Mgr.Ensure(dlCtx, entry.Spec()); err != nil {
			if dlCtx.Err() != nil {
				s.log().Info("download cancelled", "model", id)
			} else {
				s.log().Warn("download failed", "model", id, "err", err)
			}
			return
		}
		s.log().Info("download complete", "model", id)
		s.Events.Publish("models_changed", map[string]string{"model": id, "change": "downloaded"})
	}()
	return true, nil
}

// CancelDownload aborts an in-flight download. The .partial file is kept so
// a later attempt resumes.
func (s *Service) CancelDownload(id string) bool {
	s.mu.Lock()
	cancel, ok := s.downloads[id]
	s.mu.Unlock()
	if ok {
		cancel()
	}
	return ok
}

// Downloading reports whether a download for id is in flight.
func (s *Service) Downloading(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.downloads[id]
	return ok
}

// Fetch makes sure a catalog model is on disk — download and verify
// inside max_disk_mb, with the given cache origin — without loading it.
// It backs staged coordinator placements (ModelAssignment.stage). A cache
// hit returns immediately. models.ErrOverBudget when it cannot fit.
func (s *Service) Fetch(ctx context.Context, id, origin string) error {
	if err := models.ValidateID(id); err != nil {
		return err
	}
	entry, ok, err := s.Lookup(ctx, id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownModel, id)
	}
	had := s.Mgr.Has(id)
	if _, err := s.Mgr.EnsureOrigin(ctx, entry.Spec(), origin); err != nil {
		return err
	}
	if !had {
		s.log().Info("model fetched", "model", id, "origin", origin)
		s.Events.Publish("models_changed", map[string]string{"model": id, "change": "downloaded"})
	}
	return nil
}

// Load makes a model servable: cache hit or download, then runtime load and
// engine registration. Synchronous — llama-server startup takes seconds and
// callers want the outcome. No-op if already loaded.
func (s *Service) Load(ctx context.Context, id string) error {
	_, err := s.LoadInstance(ctx, id)
	return err
}

// LoadInstance is Load returning the runtime instance (startup needs it).
func (s *Service) LoadInstance(ctx context.Context, id string) (rt.Instance, error) {
	return s.LoadInstanceOrigin(ctx, id, models.OriginOperator)
}

// LoadInstanceOrigin is LoadInstance with ownership of a fresh download
// (models.OriginMesh for coordinator placements: the fetch may then only
// evict other mesh-placed models to make room).
//
// Memory admission happens after the artifact is on disk and before the
// runtime starts: the load's estimated footprint must fit the budget,
// idle instances are unloaded to make room (see admit), and ErrOverMemory
// is returned — with the file kept — when they cannot.
func (s *Service) LoadInstanceOrigin(ctx context.Context, id, origin string) (rt.Instance, error) {
	if inst, ok := s.loadedInstance(id); ok {
		return inst, nil
	}

	s.mu.Lock()
	if s.loading == nil {
		s.loading = map[string]bool{}
	}
	if s.loading[id] {
		s.mu.Unlock()
		return nil, fmt.Errorf("modelops: %s is already loading", id)
	}
	s.loading[id] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.loading, id)
		s.mu.Unlock()
	}()

	entry, ok, err := s.Lookup(ctx, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownModel, id)
	}
	pspec := entry.Spec()
	// Refuse before downloading what the installed runtime build cannot
	// serve (a decision model on a llama.cpp build without /v1/systemone).
	if err := s.CheckRuntime(ctx, entry); err != nil {
		return nil, err
	}
	art, err := s.Mgr.EnsureArtifact(ctx, pspec, origin)
	if err != nil {
		return nil, err
	}
	path := art.Path
	// The store's origin is the truth for admission ordering: a
	// coordinator re-send for a model the operator installed must not turn
	// it into a mesh-placed one that admission unloads first. (Empty for
	// file:// artifacts, which the store does not index.)
	if o := s.Mgr.Origin(id); o != "" {
		origin = o
	}
	spec := rt.ModelSpec{
		ID:            pspec.GetId(),
		Family:        pspec.GetFamily(),
		Quant:         pspec.GetQuant(),
		SHA256:        pspec.GetSha256(),
		Path:          path,
		MmprojPath:    art.MmprojPath,
		SizeBytes:     art.SizeBytes,
		ContextLength: int(pspec.GetContextLength()),
		Embeddings:    pspec.GetEmbeddings(),
		Decision:      pspec.GetDecision(),
	}

	// Footprint estimate: on-disk size (the store's, then a stat — not
	// the catalog: a local artifact may differ) at the context the runtime
	// will actually use. For a sharded model the store's figure is the
	// whole set; a stat of Path would see only part 1.
	fileBytes := art.SizeBytes
	if fileBytes == 0 {
		if fi, err := os.Stat(path); err == nil {
			fileBytes = fi.Size()
		} else {
			fileBytes = int64(pspec.GetSizeBytes())
		}
	}
	// One snapshot of the operator's plan limits for the whole load: they
	// can change under a running daemon (PUT /api/v1/limits).
	lim := s.PlanLimits()
	// Slots and context are planned from the memory budget (flockd#46):
	// admission is asked for room for the smallest layout the operator
	// accepts (the per-slot floor on one slot), and once idle models have
	// been unloaded for that much, the plan spends whatever is free on
	// slots and context, evenly. The estimate charged to the load is the
	// plan's, so admission and the launch arguments cannot disagree.
	in := memory.PlanInput{
		FileBytes: fileBytes, MinRAMMB: int64(pspec.GetMinRamMb()),
		Window: spec.ContextLength, Slots: lim.slots(s.Hardware),
		MinCtx: lim.MinContext, MaxCtx: lim.MaxContext, CtxPin: lim.ContextLength,
	}
	// The GGUF header says exactly what a context token costs and, for a
	// local model with no catalog entry, how long the training window is.
	// The file-size heuristic stays as the fallback; it is 2–4x low on
	// small GQA models (the 3B: 112 KB/token real, 30 KB guessed), which
	// planned a 262k-token cache the estimate put at 10 GB and the process
	// took 27 GB for.
	arch := ""
	if meta, err := gguf.ReadMeta(path); err == nil {
		arch = meta.Architecture
		in.KVBytesPerToken = meta.KVBytesPerToken()
		if in.Window <= 0 && meta.ContextLength > 0 {
			in.Window = meta.ContextLength
		}
	} else {
		s.log().Debug("gguf header not read; using the size heuristic", "model", id, "err", err)
	}
	if spec.Decision {
		// A decision model is planned modestly, not grown into the free
		// budget; a BERT-family encoder (Laya, Julia) is also costed as
		// what it is: no KV cache, one batch buffer (memory.DecisionPlanInput).
		in = memory.DecisionPlanInput(in, strings.Contains(strings.ToLower(arch), "bert"), strings.ToLower(spec.Family))
	}
	// Chat and embedding models start at the slot floor and follow demand
	// from there up to the ceiling (flockd#54, scale.go); a decision
	// model keeps its fixed plan.
	scalable := !spec.Decision
	if scalable {
		in.Slots = lim.floor(s.Hardware)
	}
	base := in // the plan input without the budget: what a resize re-plans from
	minEstimate := in.EstimateAt(memory.FloorContext(in))

	s.admitMu.Lock()
	defer s.admitMu.Unlock()
	if inst, ok := s.loadedInstance(id); ok {
		return inst, nil // raced with another loader while downloading
	}
	if err := s.admit(ctx, id, minEstimate); err != nil {
		s.log().Warn("model not loaded: over memory budget", "model", id, "estimate_mb", minEstimate, "err", err)
		return nil, err
	}
	in.BudgetMB, in.UsedMB = s.MemoryBudgetMB(), s.usedMB()
	plan := memory.PlanContext(in)
	estimate := plan.EstimateMB
	res := s.Budget
	res.MaxConcurrent = lim.slots(s.Hardware)
	res.Slots, res.ContextTokens = plan.Slots, plan.TotalCtx
	s.log().Info("context plan", "model", id, "slots", plan.Slots, "ctx_per_slot", plan.CtxPerSlot,
		"ctx_total", plan.TotalCtx, "kv_kb_per_token", in.KVBytesPerToken/1024,
		"decision", spec.Decision, "encoder_mb_per_token", in.EncoderMBPerToken, "estimate_mb", estimate,
		"used_mb", in.UsedMB, "budget_mb", in.BudgetMB, "squeezed", plan.Squeezed)
	// Starting the runtime reads the weights into memory and warms the
	// GPU with no request in sight: make that visible (live activity view,
	// `models_changed {change: loading}`) for as long as it takes.
	s.mu.Lock()
	if s.starting == nil {
		s.starting = map[string]time.Time{}
	}
	s.starting[id] = time.Now()
	s.mu.Unlock()
	s.Events.Publish("models_changed", map[string]string{"model": id, "change": "loading"})
	inst, err := s.Loader.Load(ctx, spec, res)
	s.mu.Lock()
	delete(s.starting, id)
	s.mu.Unlock()
	if err != nil {
		s.Events.Publish("models_changed", map[string]string{"model": id, "change": "load_failed"})
		return nil, err
	}
	s.mu.Lock()
	if s.loads == nil {
		s.loads = map[string]*loadInfo{}
	}
	s.loads[id] = &loadInfo{Origin: origin, EstimateMB: estimate, LoadedAt: time.Now(), VRAMSeq: s.vramSampleSeq,
		Limits: lim, Slots: plan.Slots, CtxPerSlot: plan.CtxPerSlot,
		Spec: spec, Plan: base, Scalable: scalable, Resized: time.Now()}
	s.mu.Unlock()
	s.Eng.RegisterSlots(spec, inst, plan.Slots)
	if s.OnLoaded != nil {
		s.OnLoaded(inst)
	}
	s.log().Info("model loaded", "model", id, "origin", origin, "estimate_mb", estimate)
	s.Events.Publish("models_changed", map[string]string{"model": id, "change": "loaded"})
	actor := activity.ActorOperator
	if origin == models.OriginMesh {
		actor = activity.ActorMesh
	}
	s.Activity.Record(activity.KindLoaded, actor, id, fmt.Sprintf("loaded %s (~%d MB)", id, estimate), "")
	return inst, nil
}

// CheckRuntime reports whether the runtime build this node runs can serve
// a catalog model, without downloading or loading anything. It returns an
// error wrapping rt.ErrRuntimeTooOld for a decision model on a llama.cpp
// build that predates decision support (the reason names both builds),
// and nil when the loader cannot tell (mock, an operator's own binary).
func (s *Service) CheckRuntime(ctx context.Context, m models.CatalogModel) error {
	sup, ok := s.Loader.(rt.ModelSupporter)
	if !ok {
		return nil
	}
	return sup.SupportsModel(ctx, rt.ModelSpec{ID: m.ID, Embeddings: m.Embeddings, Decision: m.Decision})
}

// PlanLimits are the operator's inputs to the context plan: the slot
// ceiling and the per-slot context bounds. They are the settings that
// decide a runtime's launch arguments, so changing one takes a reload of
// the model to show.
type PlanLimits struct {
	// MaxConcurrent is budget.max_concurrent as configured: the slot
	// ceiling, 0 = auto by accelerator class.
	MaxConcurrent int
	// MaxContext, MinContext and ContextLength are runtime.max_context,
	// min_context and context_length.
	MaxContext, MinContext, ContextLength int
	// MinConcurrent is budget.min_concurrent: the slots a chat model
	// starts with and shrinks back to (flockd#54). 0 = auto
	// (DefaultFloorSlots). At or above the ceiling it pins the slot count:
	// no scaling.
	MinConcurrent int
}

// DefaultFloorSlots is the auto slot floor: where a chat model starts.
// Two slots is the measured sweet spot for a lone stream of requests
// (1.44x aggregate, ~70 tok/s per request on Metal, flockd#46) at an
// eighth of the memory of the 16-slot ceiling.
const DefaultFloorSlots = 2

// floor resolves the slot floor: never above the ceiling.
func (l PlanLimits) floor(hw *typesv1.CapabilityProfile) int {
	f := l.MinConcurrent
	if f <= 0 {
		f = DefaultFloorSlots
	}
	return min(f, l.slots(hw))
}

// slots resolves the ceiling: the operator's number, or the hardware
// default when it is 0 (auto).
func (l PlanLimits) slots(hw *typesv1.CapabilityProfile) int {
	if l.MaxConcurrent > 0 {
		return l.MaxConcurrent
	}
	return memory.DefaultSlots(hw)
}

// PlanLimits returns the limits the next load plans with.
func (s *Service) PlanLimits() PlanLimits {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.planSet {
		// Constructed with the plain fields (startup, tests): adopt them.
		// Budget.MaxConcurrent there is already resolved, never 0.
		return PlanLimits{MaxConcurrent: s.Budget.MaxConcurrent, MaxContext: s.MaxContext,
			MinContext: s.MinContext, ContextLength: s.ContextLength, MinConcurrent: s.MinConcurrent}
	}
	return s.plan
}

// SetPlanLimits changes the limits for every later load. Models already
// loaded keep their layout until they are reloaded (ReloadStale).
func (s *Service) SetPlanLimits(l PlanLimits) {
	s.mu.Lock()
	s.plan, s.planSet = l, true
	s.mu.Unlock()
	s.log().Info("plan limits changed", "max_concurrent", l.MaxConcurrent, "min_concurrent", l.MinConcurrent, "max_context", l.MaxContext,
		"min_context", l.MinContext, "context_length", l.ContextLength)
}

// SlotCeiling is the resolved slot ceiling of the current plan limits.
func (s *Service) SlotCeiling() int { return s.PlanLimits().slots(s.Hardware) }

// SlotFloor is the resolved slot floor of the current plan limits.
func (s *Service) SlotFloor() int { return s.PlanLimits().floor(s.Hardware) }

// Layout is the slot and context layout a loaded model runs with.
type Layout struct {
	Slots, CtxPerSlot int
	// Stale: the plan limits changed since this model was loaded; it
	// picks the new ones up when it is next loaded.
	Stale bool
}

// Layouts reports the layout of every loaded model.
func (s *Service) Layouts() map[string]Layout {
	cur := s.PlanLimits()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Layout, len(s.loads))
	for id, li := range s.loads {
		out[id] = Layout{Slots: li.Slots, CtxPerSlot: li.CtxPerSlot, Stale: li.Limits != cur}
	}
	return out
}

// ReloadStale applies changed plan limits to the models that are loaded:
// each one whose layout was planned under other limits and that has
// nothing in flight is unloaded and loaded again (a runtime restart: the
// weights are on disk, so seconds). A busy model is left alone — never
// interrupt a request for a setting — and picks the limits up at its next
// load. The default model stays the default. It returns the ids reloaded
// and the ids skipped because they were busy.
func (s *Service) ReloadStale(ctx context.Context) (reloaded, busy []string) {
	def := s.Eng.DefaultModel()
	var stale []string
	for id, l := range s.Layouts() {
		if l.Stale {
			stale = append(stale, id)
		}
	}
	sort.Strings(stale)
	for _, id := range stale {
		s.mu.Lock()
		origin := models.OriginOperator
		if li, ok := s.loads[id]; ok && li.Origin != "" {
			origin = li.Origin
		}
		s.mu.Unlock()
		err := s.unload(ctx, id, unloadOpts{actor: activity.ActorDaemon, reason: "settings changed", idleOnly: true, reloading: true})
		if errors.Is(err, engine.ErrBusy) {
			busy = append(busy, id)
			continue
		}
		if err != nil {
			continue // unloaded by someone else meanwhile
		}
		if _, err := s.LoadInstanceOrigin(ctx, id, origin); err != nil {
			s.log().Warn("reload after a settings change failed; model left unloaded", "model", id, "err", err)
			s.Activity.Record(activity.KindUnloaded, activity.ActorDaemon, id, "unloaded "+id+": reload after a settings change failed", err.Error())
			if s.OnUnloaded != nil {
				s.OnUnloaded(id)
			}
			continue
		}
		reloaded = append(reloaded, id)
	}
	if def != "" && s.Eng.DefaultModel() != def {
		_ = s.Eng.SetDefault(def) // gone if its reload failed
	}
	return reloaded, busy
}

// Loading reports whether a load of id is in progress (download, admission
// or runtime start). The store's retention pass treats it like loaded.
func (s *Service) Loading(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loading[id]
}

// Starting lists the models whose runtime is starting right now (weights
// being read into memory), with when each start began.
func (s *Service) Starting() map[string]time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]time.Time, len(s.starting))
	for id, at := range s.starting {
		out[id] = at
	}
	return out
}

func (s *Service) loadedInstance(id string) (rt.Instance, bool) {
	for _, m := range s.Eng.Models() {
		if m.Spec.ID == id {
			return m.Instance, true
		}
	}
	return nil, false
}

// Unload removes a model from serving (the artifact stays cached). It is
// the operator's explicit request, so requests in flight on the model
// fail rather than keep it loaded.
func (s *Service) Unload(ctx context.Context, id string) error {
	return s.unload(ctx, id, unloadOpts{actor: activity.ActorOperator})
}

// Remove unloads id if it is loaded and deletes it from the store. Unlike
// Unload followed by Manager.Remove, it never reports the model as
// `cached` to the coordinator in between: the file is going away.
func (s *Service) Remove(ctx context.Context, id string) error {
	err := s.unload(ctx, id, unloadOpts{actor: activity.ActorOperator, reason: "removed", removing: true})
	if err != nil && !errors.Is(err, engine.ErrModelNotFound) {
		return err
	}
	return s.Mgr.Remove(id)
}

// unloadOpts says who is unloading and how.
type unloadOpts struct {
	actor, reason string
	// idleOnly refuses (engine.ErrBusy) when the model has requests in
	// flight — the daemon's own unloads never interrupt a request.
	idleOnly bool
	// removing suppresses the unloaded activity row and the OnUnloaded
	// (`cached`) hook: the artifact is being deleted, not kept warm.
	removing bool
	// reloading suppresses the same two: the model is coming straight
	// back (ReloadStale), so the coordinator is not told it went `cached`.
	reloading bool
}

func (s *Service) unload(ctx context.Context, id string, o unloadOpts) error {
	var entry *engine.ModelEntry
	if o.idleOnly {
		var err error
		if entry, err = s.Eng.UnregisterIdle(id); err != nil {
			return err
		}
	} else if entry = s.Eng.Unregister(id); entry == nil {
		return fmt.Errorf("%w: %q not loaded", engine.ErrModelNotFound, id)
	}
	s.mu.Lock()
	delete(s.loads, id)
	s.mu.Unlock()
	shutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := entry.Instance.Shutdown(shutCtx); err != nil {
		s.log().Warn("unload shutdown", "model", id, "err", err)
	}
	s.log().Info("model unloaded", "model", id, "actor", o.actor, "reason", o.reason)
	s.Events.Publish("models_changed", map[string]string{"model": id, "change": "unloaded"})
	if o.removing || o.reloading {
		return nil
	}
	s.Activity.Record(activity.KindUnloaded, o.actor, id, "unloaded "+id, o.reason)
	if s.OnUnloaded != nil {
		s.OnUnloaded(id)
	}
	return nil
}

// SetDefault switches the model served when a request names none.
func (s *Service) SetDefault(id string) error {
	if err := s.Eng.SetDefault(id); err != nil {
		return err
	}
	s.Events.Publish("models_changed", map[string]string{"model": id, "change": "default"})
	return nil
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}
