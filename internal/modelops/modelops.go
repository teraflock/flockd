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
	"sync"
	"sync/atomic"
	"time"

	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"

	"github.com/teraflock/flockd/internal/activity"
	"github.com/teraflock/flockd/internal/engine"
	"github.com/teraflock/flockd/internal/events"
	"github.com/teraflock/flockd/internal/memory"
	"github.com/teraflock/flockd/internal/models"
	rt "github.com/teraflock/flockd/internal/runtime"
)

// ErrUnknownModel is returned when an id is in neither the catalog nor the
// local cache.
var ErrUnknownModel = errors.New("modelops: model not in catalog")

// ErrDownloadRunning is returned when a download for the id is in flight.
var ErrDownloadRunning = errors.New("modelops: download already running")

// catalogTTL is how long a fetched catalog is trusted before a background
// refetch. Catalog reads never block on the network once one copy is held.
const catalogTTL = 15 * time.Minute

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
	// MaxContext mirrors runtime.max_context (0 = no cap).
	MaxContext int
	// ContextLength is the operator's runtime.context_length override,
	// which the footprint estimate must reflect (0 = catalog/default).
	ContextLength int
	// OnUnloaded is called after any unload (operator, idle, memory
	// pressure) with the model id; the assign service reports `cached` to
	// the coordinator from it. May be nil.
	OnUnloaded func(id string)

	memBudget  atomic.Int64 // configured budget.max_ram_mb; 0 = auto
	idleUnload atomic.Int64 // nanoseconds; 0 = never

	// admitMu serialises admission + runtime load so two loads cannot both
	// pass the same headroom check. Downloads happen outside it.
	admitMu sync.Mutex

	mu        sync.Mutex
	catalog   *models.Catalog
	fetchedAt time.Time
	downloads map[string]context.CancelFunc
	loading   map[string]bool
	loads     map[string]*loadInfo
}

// Catalog returns the model catalog, cached for catalogTTL. refresh forces a
// refetch.
func (s *Service) Catalog(ctx context.Context, refresh bool) (*models.Catalog, error) {
	s.mu.Lock()
	if s.catalog != nil && !refresh && time.Since(s.fetchedAt) < catalogTTL {
		c := s.catalog
		s.mu.Unlock()
		return c, nil
	}
	s.mu.Unlock()

	c, err := models.LoadCatalog(ctx, s.ManifestPath, s.ManifestURL, nil)
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
	s.catalog = c
	s.fetchedAt = time.Now()
	s.mu.Unlock()
	if s.Mgr != nil {
		s.Mgr.Reconcile(c)
	}
	return c, nil
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
	cat, err := s.Catalog(ctx, false)
	if err != nil {
		return false, err
	}
	entry, ok := cat.Find(id)
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

	cat, err := s.Catalog(ctx, false)
	if err != nil {
		return nil, err
	}
	entry, ok := cat.Find(id)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownModel, id)
	}
	pspec := entry.Spec()
	path, err := s.Mgr.EnsureOrigin(ctx, pspec, origin)
	if err != nil {
		return nil, err
	}
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
		ContextLength: int(pspec.GetContextLength()),
		Embeddings:    pspec.GetEmbeddings(),
	}

	// Footprint estimate: file size (stat, not the catalog — a local
	// artifact may differ) at the context the runtime will actually use.
	var fileBytes int64
	if fi, err := os.Stat(path); err == nil {
		fileBytes = fi.Size()
	} else {
		fileBytes = int64(pspec.GetSizeBytes())
	}
	ctxLen := memory.ResolveContext(s.ContextLength, spec.ContextLength, s.MaxContext)
	estimate := memory.EstimateMB(fileBytes, int64(pspec.GetMinRamMb()), ctxLen, s.Budget.MaxConcurrent)

	s.admitMu.Lock()
	defer s.admitMu.Unlock()
	if inst, ok := s.loadedInstance(id); ok {
		return inst, nil // raced with another loader while downloading
	}
	if err := s.admit(ctx, id, estimate); err != nil {
		s.log().Warn("model not loaded: over memory budget", "model", id, "estimate_mb", estimate, "err", err)
		return nil, err
	}
	inst, err := s.Loader.Load(ctx, spec, s.Budget)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.loads == nil {
		s.loads = map[string]*loadInfo{}
	}
	s.loads[id] = &loadInfo{Origin: origin, EstimateMB: estimate}
	s.mu.Unlock()
	s.Eng.Register(spec, inst)
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

// Loading reports whether a load of id is in progress (download, admission
// or runtime start). The store's retention pass treats it like loaded.
func (s *Service) Loading(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loading[id]
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
	if o.removing {
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
