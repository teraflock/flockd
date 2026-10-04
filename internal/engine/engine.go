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
	"github.com/teraflock/flockd/internal/live"
	rt "github.com/teraflock/flockd/internal/runtime"
	"github.com/teraflock/flockd/internal/telemetry"
)

// payoutMicroPerToken is the standalone-mode simulated payout: 55
// micro-credits per completion token (~= the "small" class node rate from
// SPEC §7 at 1 credit = $0.000001). Real payouts come from the ledger.
const payoutMicroPerToken = 55

// ErrModelNotFound is returned for unknown model ids.
var ErrModelNotFound = errors.New("engine: model not loaded")

// ErrDecisionModel is returned when a chat, completion or embedding
// request names a decision model. The catalog flag decides, not what the
// weights could do: a decision model is served by /v1/systemone only.
var ErrDecisionModel = errors.New("engine: decision model")

// DecisionModelError is the error for a generation or embedding request
// aimed at decision model id. It wraps ErrDecisionModel and reads exactly
// like the gateway's refusal.
func DecisionModelError(id string) error {
	return &decisionModelError{id: id}
}

type decisionModelError struct{ id string }

func (e *decisionModelError) Error() string {
	return fmt.Sprintf("model %q is a decision model: use POST /v1/systemone", e.id)
}

func (*decisionModelError) Unwrap() error { return ErrDecisionModel }

// ErrNotChatModel is returned by SetDefault for a decision or embedding
// model: the default answers chat requests that name no model, so only a
// chat model can be it.
var ErrNotChatModel = errors.New("engine: not a chat model")

type notChatError struct{ msg string }

func (e *notChatError) Error() string { return e.msg }
func (*notChatError) Unwrap() error   { return ErrNotChatModel }

// chatModel reports whether a model can be the default.
func chatModel(spec rt.ModelSpec) bool { return !spec.Decision && !spec.Embeddings }

// ErrBusy is returned by UnregisterIdle when the model has requests in
// flight (or waiting in admission) and so must not be unloaded.
var ErrBusy = errors.New("engine: model busy")

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

	// inflight counts requests between lookup (under e.mu) and stream
	// end — including ones still waiting in governor admission — so that
	// UnregisterIdle, which reads it under e.mu, cannot race a request
	// that has already been routed to this instance.
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
	// reqs is the live view: requests in flight and recently finished.
	reqs *live.Tracker

	mu        sync.RWMutex
	models    map[string]*ModelEntry
	defaultID string
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
		reqs:   live.New(0),
		models: map[string]*ModelEntry{},
	}
}

// Requests is the live request tracker: what is running on the runtimes
// right now and what ran recently (GET /api/v1/requests). Set its Events
// hub before serving to get request_started / request_finished events.
func (e *Engine) Requests() *live.Tracker { return e.reqs }

// kindName is the live-view name of a request kind.
func kindName(k rt.Kind) string {
	switch k {
	case rt.KindCompletion:
		return live.KindCompletion
	case rt.KindEmbedding:
		return live.KindEmbedding
	case rt.KindDecision:
		return live.KindDecision
	default:
		return live.KindChat
	}
}

// outcomeOf is the live-view outcome of a request that ended with err.
func outcomeOf(err error) string {
	switch {
	case err == nil:
		return live.OutcomeOK
	case rt.IsInvalidInput(err):
		return live.OutcomeInvalidInput
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return live.OutcomeCancelled
	default:
		return live.OutcomeError
	}
}

// Register adds a loaded model; the first chat model registered becomes
// the default. A decision or embedding model never does: the default is
// what answers a chat request that names no model, and with only such
// models loaded there is simply no default.
func (e *Engine) Register(spec rt.ModelSpec, inst rt.Instance) {
	e.mu.Lock()
	defer e.mu.Unlock()
	entry := &ModelEntry{Spec: spec, Instance: inst, LoadedAt: time.Now()}
	entry.lastUsed.Store(entry.LoadedAt.UnixNano())
	e.models[spec.ID] = entry
	if e.defaultID == "" && chatModel(spec) {
		e.defaultID = spec.ID
	}
}

// Unregister removes a model (eviction); callers shut the instance down.
// Requests in flight on it fail when the instance goes away — use
// UnregisterIdle when that is not acceptable.
func (e *Engine) Unregister(id string) *ModelEntry {
	e.mu.Lock()
	defer e.mu.Unlock()
	entry := e.models[id]
	e.removeLocked(id)
	return entry
}

// UnregisterIdle removes a model only if nothing is in flight on it. The
// in-flight check and the removal happen under the same lock the request
// path takes to route to the model, so a request cannot slip in between:
// after this returns, either the model is gone and no request holds it,
// or ErrBusy says a request does. Daemon-initiated unloads (memory
// admission, idle unload) use this; the operator's explicit unload keeps
// Unregister's forced semantics.
func (e *Engine) UnregisterIdle(id string) (*ModelEntry, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	entry, ok := e.models[id]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrModelNotFound, id)
	}
	if n := entry.inflight.Load(); n > 0 {
		return nil, fmt.Errorf("%w: %q has %d request(s) in flight", ErrBusy, id, n)
	}
	e.removeLocked(id)
	return entry, nil
}

func (e *Engine) removeLocked(id string) {
	delete(e.models, id)
	if e.defaultID == id {
		// The longest-loaded chat model takes over; none = no default.
		e.defaultID = ""
		var oldest time.Time
		for mid, m := range e.models {
			if !chatModel(m.Spec) {
				continue
			}
			if e.defaultID == "" || m.LoadedAt.Before(oldest) || (m.LoadedAt.Equal(oldest) && mid < e.defaultID) {
				e.defaultID, oldest = mid, m.LoadedAt
			}
		}
	}
}

// SetDefault names the model used when a request omits one. The model must
// be loaded — a default pointing at nothing would turn every bare request
// into a 404.
func (e *Engine) SetDefault(id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	m, ok := e.models[id]
	if !ok {
		return fmt.Errorf("%w: %q", ErrModelNotFound, id)
	}
	switch {
	case m.Spec.Decision:
		return &notChatError{fmt.Sprintf("%q is a decision model (served by /v1/systemone) and cannot be the default: the default answers chat requests that name no model", id)}
	case m.Spec.Embeddings:
		return &notChatError{fmt.Sprintf("%q is an embedding model and cannot be the default: the default answers chat requests that name no model", id)}
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
		return nil, notFound(id)
	}
	return m, nil
}

// notFound is ErrModelNotFound for id; an empty id means the request named
// no model and there is no default to fall back on.
func notFound(id string) error {
	if id == "" {
		return fmt.Errorf("%w: the request names no model and no default chat model is loaded", ErrModelNotFound)
	}
	return fmt.Errorf("%w: %q", ErrModelNotFound, id)
}

// acquire is lookup plus the in-flight increment, both under the read
// lock, so an UnregisterIdle that observes inflight == 0 is guaranteed no
// request has been routed to the entry (see ModelEntry.inflight).
func (e *Engine) acquire(model string) (*ModelEntry, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	id := model
	if id == "" {
		id = e.defaultID
	}
	m, ok := e.models[id]
	if !ok {
		return nil, notFound(id)
	}
	m.inflight.Add(1)
	return m, nil
}

// Complete runs one request through admission, the runtime, and telemetry.
func (e *Engine) Complete(ctx context.Context, req rt.CompletionRequest) (rt.TokenStream, error) {
	entry, err := e.acquire(req.Model)
	if err != nil {
		return nil, err
	}
	// A decision model answers typed decisions and nothing else. Refused
	// here, before admission and the runtime, so the local API and the
	// tunnel both get one clear error instead of whatever the runtime
	// makes of a chat request (llama-server: a 500 about logits).
	if entry.Spec.Decision && req.Kind != rt.KindDecision {
		entry.inflight.Add(-1)
		return nil, DecisionModelError(entry.Spec.ID)
	}

	release := func() {}
	runCtx := ctx
	if e.admit != nil {
		var admitted context.Context
		admitted, release, err = e.admit.Admit(ctx, req.ID)
		if err != nil {
			entry.inflight.Add(-1)
			return nil, err
		}
		runCtx = admitted
	}

	if e.touch != nil {
		e.touch(entry.Spec.ID)
	}
	entry.lastUsed.Store(time.Now().UnixNano())
	e.stats.RequestStarted()

	// Tracked from here: the request has passed admission and is about to
	// use the runtime. What was refused before this point did no work.
	h := e.reqs.Start(entry.Spec.ID, kindName(req.Kind), req.Origin)
	stream, err := entry.Instance.Complete(runCtx, req)
	if err != nil {
		h.Finish(outcomeOf(err), 0, 0)
		entry.inflight.Add(-1)
		e.stats.RequestFinished()
		release()
		return nil, err
	}
	return &meteredStream{inner: stream, eng: e, entry: entry, release: release, live: h, outcome: live.OutcomeCancelled}, nil
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
	// live is the request's row in the live view. outcome is what it ends
	// as: "cancelled" until the stream says otherwise — a stream closed
	// before its final chunk was abandoned by the caller.
	live     *live.Handle
	outcome  string
	gotUsage bool
}

func (s *meteredStream) finish() {
	s.once.Do(func() {
		completion := -1 // no usage chunk: the streamed count stands
		if s.gotUsage {
			completion = s.usage.CompletionTokens
		}
		s.live.Finish(s.outcome, s.usage.PromptTokens, completion)
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
		} else {
			s.outcome = outcomeOf(err)
		}
		return c, err
	}
	if c.TokenCount > 0 {
		s.eng.stats.RecordTokens(c.TokenCount)
		s.live.AddTokens(c.TokenCount)
	}
	if c.Usage != nil {
		s.usage, s.gotUsage = *c.Usage, true
	}
	switch {
	case c.Err != "" || c.FinishReason == "error":
		s.outcome = live.OutcomeError
	case c.FinishReason == "cancelled":
		s.outcome = live.OutcomeCancelled
	case c.Done:
		s.outcome = live.OutcomeOK
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
