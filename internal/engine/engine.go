// Package engine routes completion requests to loaded runtime Instances,
// applying governor admission and telemetry accounting. It is the single
// serving funnel shared by the local OpenAI API and the tunnel dispatcher.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/teraflock/flockd/internal/governor"
	rt "github.com/teraflock/flockd/internal/runtime"
	"github.com/teraflock/flockd/internal/telemetry"
)

// payoutMicroPerToken is the standalone-mode simulated payout: 55
// micro-credits per completion token (~= the "small" class node rate from
// SPEC §7 at 1 credit = $0.000001). Real payouts come from the ledger.
const payoutMicroPerToken = 55

// ErrModelNotFound is returned for unknown model ids.
var ErrModelNotFound = errors.New("engine: model not loaded")

// Admitter gates requests (usually *governor.Governor).
type Admitter interface {
	Admit(ctx context.Context, id string) (context.Context, func(), error)
}

// ModelEntry pairs a loaded instance with its metadata.
type ModelEntry struct {
	Spec     rt.ModelSpec
	Instance rt.Instance
	// LoadedAt is when the instance was registered.
	LoadedAt time.Time

	inflight atomic.Int64
	lastUsed atomic.Int64 // unix nanos of the most recent request start
}

// Usage is the per-model activity view idle unload and memory admission
// read: when it last started serving a request (LoadedAt if never) and
// how many requests it is serving right now.
type Usage struct {
	LastUsed time.Time
	Inflight int
}

// Engine implements tunnel.Engine and backs localapi.
type Engine struct {
	admit Admitter
	stats *telemetry.Stats
	touch func(modelID string) // models.Manager LRU recency hook, may be nil

	mu          sync.RWMutex
	models      map[string]*ModelEntry
	defaultID   string
	totalErrors int64
}

// New builds an Engine. admit may be nil (no gating), stats may be nil.
func New(admit Admitter, stats *telemetry.Stats, touch func(string)) *Engine {
	if stats == nil {
		stats = telemetry.NewStats()
	}
	return &Engine{
		admit:  admit,
		stats:  stats,
		touch:  touch,
		models: map[string]*ModelEntry{},
	}
}

// Register adds a loaded model; the first registered becomes the default.
func (e *Engine) Register(spec rt.ModelSpec, inst rt.Instance) {
	e.mu.Lock()
	defer e.mu.Unlock()
	entry := &ModelEntry{Spec: spec, Instance: inst, LoadedAt: time.Now()}
	entry.lastUsed.Store(entry.LoadedAt.UnixNano())
	e.models[spec.ID] = entry
	if e.defaultID == "" {
		e.defaultID = spec.ID
	}
}

// Unregister removes a model (eviction); callers shut the instance down.
func (e *Engine) Unregister(id string) *ModelEntry {
	e.mu.Lock()
	defer e.mu.Unlock()
	entry := e.models[id]
	delete(e.models, id)
	if e.defaultID == id {
		e.defaultID = ""
		for mid := range e.models {
			e.defaultID = mid
			break
		}
	}
	return entry
}

// SetDefault names the model used when a request omits one. The model must
// be loaded — a default pointing at nothing would turn every bare request
// into a 404.
func (e *Engine) SetDefault(id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.models[id]; !ok {
		return fmt.Errorf("%w: %q", ErrModelNotFound, id)
	}
	e.defaultID = id
	return nil
}

// DefaultModel returns the fallback model id.
func (e *Engine) DefaultModel() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.defaultID
}

// Models lists loaded entries.
func (e *Engine) Models() []*ModelEntry {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]*ModelEntry, 0, len(e.models))
	for _, m := range e.models {
		out = append(out, m)
	}
	return out
}

// Stats exposes the telemetry accumulator.
func (e *Engine) Stats() *telemetry.Stats { return e.stats }

// Usage reports a loaded model's last request time and in-flight count.
func (e *Engine) Usage(id string) (Usage, bool) {
	e.mu.RLock()
	m, ok := e.models[id]
	e.mu.RUnlock()
	if !ok {
		return Usage{}, false
	}
	return Usage{LastUsed: time.Unix(0, m.lastUsed.Load()), Inflight: int(m.inflight.Load())}, true
}

func (e *Engine) lookup(model string) (*ModelEntry, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	id := model
	if id == "" {
		id = e.defaultID
	}
	m, ok := e.models[id]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrModelNotFound, id)
	}
	return m, nil
}

// Complete runs one request through admission, the runtime, and telemetry.
func (e *Engine) Complete(ctx context.Context, req rt.CompletionRequest) (rt.TokenStream, error) {
	entry, err := e.lookup(req.Model)
	if err != nil {
		return nil, err
	}

	release := func() {}
	runCtx := ctx
	if e.admit != nil {
		var admitted context.Context
		admitted, release, err = e.admit.Admit(ctx, req.ID)
		if err != nil {
			return nil, err
		}
		runCtx = admitted
	}

	if e.touch != nil {
		e.touch(entry.Spec.ID)
	}
	entry.lastUsed.Store(time.Now().UnixNano())
	entry.inflight.Add(1)
	e.stats.RequestStarted()

	stream, err := entry.Instance.Complete(runCtx, req)
	if err != nil {
		entry.inflight.Add(-1)
		e.stats.RequestFinished()
		release()
		return nil, err
	}
	return &meteredStream{inner: stream, eng: e, entry: entry, release: release}, nil
}

// Health proxies the default (or named) instance health.
func (e *Engine) Health(ctx context.Context, model string) (rt.Stats, error) {
	entry, err := e.lookup(model)
	if err != nil {
		return rt.Stats{}, err
	}
	return entry.Instance.Health(ctx)
}

// meteredStream instruments token flow and releases admission exactly once
// when the stream ends (EOF, error, or Close).
type meteredStream struct {
	inner   rt.TokenStream
	eng     *Engine
	entry   *ModelEntry
	release func()
	once    sync.Once
	usage   rt.Usage
}

func (s *meteredStream) finish() {
	s.once.Do(func() {
		s.entry.inflight.Add(-1)
		s.eng.stats.RequestFinished()
		s.eng.stats.RecordRequest(s.usage.CompletionTokens, int64(s.usage.CompletionTokens)*payoutMicroPerToken)
		s.release()
	})
}

func (s *meteredStream) Recv() (rt.Chunk, error) {
	c, err := s.inner.Recv()
	if err != nil {
		if errors.Is(err, io.EOF) {
			s.finish()
		}
		return c, err
	}
	if c.TokenCount > 0 {
		s.eng.stats.RecordTokens(c.TokenCount)
	}
	if c.Usage != nil {
		s.usage = *c.Usage
	}
	return c, nil
}

func (s *meteredStream) Close() error {
	err := s.inner.Close()
	s.finish()
	return err
}

var _ rt.TokenStream = (*meteredStream)(nil)
var _ Admitter = (*governor.Governor)(nil)
