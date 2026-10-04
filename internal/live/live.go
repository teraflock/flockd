// Package live tracks what the node is doing right now and what it did a
// moment ago: the requests in flight on each model and a bounded ring of
// the ones that finished. It answers "the GPU spiked two minutes ago —
// was that flockd, which model, and for whom?".
//
// It records facts about a request, never its content: model, kind,
// origin, times, token counts and an outcome word. No prompt, no output,
// no error text (runtime errors can quote input) and no identifier from
// outside the node — the id is a counter local to this process.
//
// The hot path is one mutex-guarded map insert at start, one atomic add
// per token chunk, and one ring write at the end.
package live

import (
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/teraflock/flockd/internal/events"
)

// Kinds of request (api/openapi.yaml RequestKind).
const (
	KindChat       = "chat"
	KindCompletion = "completion"
	KindEmbedding  = "embedding"
	KindDecision   = "decision"
)

// Origins: who asked (api/openapi.yaml RequestOrigin).
const (
	// OriginLocal is the local API on 127.0.0.1 (/v1/*): the operator's
	// own tools.
	OriginLocal = "local"
	// OriginMesh is a coordinator dispatch: a customer request, or a
	// canary — a node cannot tell the two apart, by design.
	OriginMesh = "mesh"
	// OriginChallenge is a fingerprint challenge from the coordinator.
	OriginChallenge = "challenge"
)

// Outcomes of a finished request (api/openapi.yaml RequestOutcome).
const (
	OutcomeOK           = "ok"
	OutcomeError        = "error"
	OutcomeCancelled    = "cancelled"
	OutcomeInvalidInput = "invalid_input"
)

// SSE event names.
const (
	EventStarted  = "request_started"
	EventFinished = "request_finished"
)

// DefaultCapacity is how many finished requests a Tracker keeps.
const DefaultCapacity = 200

// InFlight is a request being served. Tokens is what has been generated
// so far (0 for embeddings and decisions, which generate nothing).
type InFlight struct {
	ID        string    `json:"id"`
	Model     string    `json:"model"`
	Kind      string    `json:"kind"`
	Origin    string    `json:"origin"`
	StartedAt time.Time `json:"started_at"`
	ElapsedMS int64     `json:"elapsed_ms"`
	Tokens    int       `json:"tokens"`
}

// Finished is a request that ended.
type Finished struct {
	ID               string    `json:"id"`
	Model            string    `json:"model"`
	Kind             string    `json:"kind"`
	Origin           string    `json:"origin"`
	StartedAt        time.Time `json:"started_at"`
	DurationMS       int64     `json:"duration_ms"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	Outcome          string    `json:"outcome"`
}

// Tracker holds the in-flight set and the recent ring. Safe for
// concurrent use; a nil *Tracker tracks nothing, so callers need no nil
// checks.
type Tracker struct {
	// Events, when set before use, receives request_started and
	// request_finished.
	Events *events.Hub

	mu       sync.Mutex
	seq      uint64
	inflight map[uint64]*Handle
	ring     []Finished
	next     int
	full     bool
	clock    func() time.Time
}

// New builds a tracker keeping the last n finished requests
// (DefaultCapacity when n <= 0).
func New(n int) *Tracker {
	if n <= 0 {
		n = DefaultCapacity
	}
	return &Tracker{inflight: map[uint64]*Handle{}, ring: make([]Finished, n), clock: time.Now}
}

// Handle is one in-flight request. A nil *Handle is inert.
type Handle struct {
	t      *Tracker
	seq    uint64
	model  string
	kind   string
	origin string
	start  time.Time
	tokens atomic.Int64
	done   atomic.Bool
}

func (h *Handle) id() string { return "r" + strconv.FormatUint(h.seq, 10) }

// Start records a request entering the runtime.
func (t *Tracker) Start(model, kind, origin string) *Handle {
	if t == nil {
		return nil
	}
	if origin == "" {
		origin = OriginLocal
	}
	t.mu.Lock()
	t.seq++
	h := &Handle{t: t, seq: t.seq, model: model, kind: kind, origin: origin, start: t.clock()}
	t.inflight[h.seq] = h
	hub := t.Events
	t.mu.Unlock()
	hub.Publish(EventStarted, h.snapshot(h.start))
	return h
}

// AddTokens counts generated tokens as they stream.
func (h *Handle) AddTokens(n int) {
	if h != nil && n > 0 {
		h.tokens.Add(int64(n))
	}
}

func (h *Handle) snapshot(now time.Time) InFlight {
	return InFlight{
		ID: h.id(), Model: h.model, Kind: h.kind, Origin: h.origin, StartedAt: h.start,
		ElapsedMS: now.Sub(h.start).Milliseconds(), Tokens: int(h.tokens.Load()),
	}
}

// Finish moves the request to the recent ring. Only the first call
// counts. completionTokens < 0 means "unknown": the streamed count is
// used (a cancelled generation has no usage chunk).
func (h *Handle) Finish(outcome string, promptTokens, completionTokens int) {
	if h == nil || !h.done.CompareAndSwap(false, true) {
		return
	}
	if completionTokens < 0 {
		completionTokens = int(h.tokens.Load())
	}
	t := h.t
	t.mu.Lock()
	f := Finished{
		ID: h.id(), Model: h.model, Kind: h.kind, Origin: h.origin, StartedAt: h.start,
		DurationMS:   t.clock().Sub(h.start).Milliseconds(),
		PromptTokens: promptTokens, CompletionTokens: completionTokens, Outcome: outcome,
	}
	delete(t.inflight, h.seq)
	t.ring[t.next] = f
	t.next = (t.next + 1) % len(t.ring)
	if t.next == 0 {
		t.full = true
	}
	hub := t.Events
	t.mu.Unlock()
	hub.Publish(EventFinished, f)
}

// InFlight lists the requests being served, oldest first.
func (t *Tracker) InFlight() []InFlight {
	if t == nil {
		return []InFlight{}
	}
	t.mu.Lock()
	now := t.clock()
	out := make([]InFlight, 0, len(t.inflight))
	for _, h := range t.inflight {
		out = append(out, h.snapshot(now))
	}
	t.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if !out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].StartedAt.Before(out[j].StartedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Recent lists finished requests, newest first, at most limit (<= 0: all
// that are kept).
func (t *Tracker) Recent(limit int) []Finished {
	if t == nil {
		return []Finished{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	n := t.next
	if t.full {
		n = len(t.ring)
	}
	if limit > 0 && limit < n {
		n = limit
	}
	out := make([]Finished, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, t.ring[(t.next-i+len(t.ring))%len(t.ring)])
	}
	return out
}

// Counts is the in-flight tally of one model.
type Counts struct {
	Total  int
	ByKind map[string]int
}

// CountsByModel tallies the in-flight requests per model.
func (t *Tracker) CountsByModel() map[string]Counts {
	out := map[string]Counts{}
	if t == nil {
		return out
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, h := range t.inflight {
		c := out[h.model]
		if c.ByKind == nil {
			c.ByKind = map[string]int{}
		}
		c.Total++
		c.ByKind[h.kind]++
		out[h.model] = c
	}
	return out
}
