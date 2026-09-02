// Package assign executes the coordinator's ModelAssignment pushes on the
// operator's terms (SPEC §4.1, §4.4; plan 05).
//
// The mesh may fill this node with models — download, load, and later
// evict what it placed — but only inside the operator's consent:
//
//   - the [models] mesh_managed switch (default on; off = decline all),
//   - max_disk_mb (a placement that cannot fit even after evicting other
//     mesh-placed models is declined, never forced),
//   - pin (never evicted) and exclude (never assigned),
//   - and the operator's own models, which the mesh cannot evict at all
//     (cache entries carry an origin; only "mesh" ones are the mesh's).
//
// Every decision is reported back over the tunnel as a ModelState:
// "assigned" (accepted, queued), "downloading", "ready", "declined"
// (policy), "failed" (error), "evicted". The coordinator uses declined /
// failed to back off, and the heartbeat carries assigned/downloading so
// placement knows a replica is on its way.
package assign

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	tunnelv1 "github.com/teraflock/proto/gen/go/flock/tunnel/v1"
	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"

	"github.com/teraflock/flockd/internal/engine"
	"github.com/teraflock/flockd/internal/events"
	"github.com/teraflock/flockd/internal/modelops"
	"github.com/teraflock/flockd/internal/models"
)

// States reported to the coordinator and shown in the local API.
const (
	StateAssigned    = "assigned"
	StateDownloading = "downloading"
	StateReady       = "ready"
	StateDeclined    = "declined"
	StateFailed      = "failed"
	StateEvicted     = "evicted"
)

// failedRetention keeps a failed/declined record visible in the local API
// (and out of the heartbeat) before it is forgotten.
const failedRetention = 10 * time.Minute

// batteryPoll is how often a queued download re-checks power (a var so
// tests can shorten it).
var batteryPoll = 30 * time.Second

// Policy is the operator's standing consent, read fresh per decision so a
// dashboard toggle applies to the next assignment without a restart.
type Policy struct {
	MeshManaged bool
	Exclude     []string
	Pinned      []string
}

// Pending is one assignment the node is working on or has concluded.
type Pending struct {
	ID    string    `json:"id"`
	State string    `json:"state"`
	Since time.Time `json:"since"`
	// Error is the failure or refusal reason (failed/declined only).
	Error string `json:"error,omitempty"`
}

// Service applies assignments. Nil Ops (mock runtime) declines everything
// with a clear reason so the coordinator backs off instead of retrying.
type Service struct {
	Ops *modelops.Service
	Mgr *models.Manager
	Eng *engine.Engine
	// OnBattery reports power state; downloads wait while true.
	OnBattery func() bool
	// Policy returns the current consent. Required.
	Policy func() Policy
	// Events receives model_assignment events. May be nil.
	Events *events.Hub
	Log    *slog.Logger

	mu       sync.Mutex
	pending  map[string]*Pending
	queue    []string // ids in StateAssigned, FIFO
	wake     chan struct{}
	reporter func(*typesv1.ModelState) error
	running  bool
}

// SetReporter installs the tunnel send used for ModelStateUpdate messages
// (swapped on every (re)connect). Reports while disconnected are dropped;
// the next Hello/heartbeat carries the live states anyway.
func (s *Service) SetReporter(fn func(*typesv1.ModelState) error) {
	s.mu.Lock()
	s.reporter = fn
	s.mu.Unlock()
}

// Run starts the worker; returns when ctx ends.
func (s *Service) Run(ctx context.Context) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running = true
	if s.wake == nil {
		s.wake = make(chan struct{}, 1)
	}
	s.mu.Unlock()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-time.After(time.Minute):
			s.expire()
		}
		for {
			id, ok := s.next()
			if !ok {
				break
			}
			s.process(ctx, id)
		}
	}
}

// Apply handles one ModelAssignment: evictions first (they free the disk
// the assignments need), then admission of each assignment under the
// policy. Idempotent: a repeated push for a model already ready or in
// flight is acknowledged, not restarted.
func (s *Service) Apply(ctx context.Context, ma *tunnelv1.ModelAssignment) {
	for _, id := range ma.GetEvictModelIds() {
		s.evict(ctx, id)
	}
	for _, spec := range ma.GetAssign() {
		s.admit(ctx, spec)
	}
	s.kick()
}

func (s *Service) admit(ctx context.Context, spec *typesv1.ModelSpec) {
	id := spec.GetId()
	if err := models.ValidateID(id); err != nil {
		s.conclude(id, StateDeclined, "invalid model id")
		return
	}
	if s.loaded(id) {
		s.report(&typesv1.ModelState{ModelId: id, State: StateReady})
		return
	}
	s.mu.Lock()
	if p, ok := s.pending[id]; ok && (p.State == StateAssigned || p.State == StateDownloading) {
		s.mu.Unlock()
		return // already on it
	}
	s.mu.Unlock()

	pol := s.Policy()
	switch {
	case s.Ops == nil:
		s.conclude(id, StateDeclined, "runtime has no model store (mock runtime)")
		return
	case !pol.MeshManaged:
		s.conclude(id, StateDeclined, "mesh-managed models are off on this node")
		return
	case slices.Contains(pol.Exclude, id):
		s.conclude(id, StateDeclined, "model is in models.exclude")
		return
	}
	cat, err := s.Ops.Catalog(ctx, false)
	if err != nil {
		s.conclude(id, StateFailed, "catalog unavailable: "+err.Error())
		return
	}
	entry, ok := cat.Find(id)
	if !ok {
		s.conclude(id, StateDeclined, "model not in this node's catalog")
		return
	}
	if want := spec.GetSha256(); want != "" && entry.SHA256 != want {
		// The coordinator and this node disagree on what the artifact is.
		// Refusing is the hash-pinning story (SPEC §6) doing its job.
		s.conclude(id, StateFailed, "catalog sha256 differs from the coordinator's")
		return
	}
	s.mu.Lock()
	if s.pending == nil {
		s.pending = map[string]*Pending{}
	}
	s.pending[id] = &Pending{ID: id, State: StateAssigned, Since: time.Now()}
	s.queue = append(s.queue, id)
	s.mu.Unlock()
	s.report(&typesv1.ModelState{ModelId: id, State: StateAssigned})
	s.publish(id, StateAssigned, "")
}

// evict drops a mesh-placed model; anything else is the operator's and is
// left alone (logged, not reported — the coordinator sees it stay ready).
func (s *Service) evict(ctx context.Context, id string) {
	if s.Mgr == nil || s.Ops == nil {
		return
	}
	s.mu.Lock()
	if p, ok := s.pending[id]; ok && (p.State == StateAssigned || p.State == StateDownloading) {
		// Cancel a queued/in-flight placement; the worker sees the state.
		p.State = StateEvicted
		p.Since = time.Now()
		s.queue = slices.DeleteFunc(s.queue, func(q string) bool { return q == id })
		s.mu.Unlock()
		if s.Ops.Downloading(id) {
			s.Ops.CancelDownload(id)
		}
		s.report(&typesv1.ModelState{ModelId: id, State: StateEvicted})
		return
	}
	s.mu.Unlock()
	origin := s.Mgr.Origin(id)
	if origin == "" && !s.loaded(id) {
		return // nothing here
	}
	if origin != models.OriginMesh {
		s.log().Warn("ignoring mesh eviction of an operator-owned model", "model", id)
		return
	}
	if s.pinned(id) {
		s.log().Warn("ignoring mesh eviction of a pinned model", "model", id)
		return
	}
	if s.loaded(id) {
		if err := s.Ops.Unload(ctx, id); err != nil {
			s.log().Warn("evict: unload failed", "model", id, "err", err)
		}
	}
	if err := s.Mgr.Remove(id); err != nil {
		s.log().Warn("evict: remove failed", "model", id, "err", err)
		return
	}
	s.log().Info("model evicted on mesh request", "model", id)
	s.publish(id, StateEvicted, "")
	s.report(&typesv1.ModelState{ModelId: id, State: StateEvicted})
}

// process runs one queued assignment to ready/failed/declined.
func (s *Service) process(ctx context.Context, id string) {
	// Wait for AC power: a multi-GB download is exactly what the operator
	// does not want on battery, whatever their serve policy says.
	for s.OnBattery != nil && s.OnBattery() {
		if !s.stillQueued(id) {
			return
		}
		s.log().Info("assignment waiting for AC power", "model", id)
		select {
		case <-ctx.Done():
			return
		case <-time.After(batteryPoll):
		}
	}
	if !s.setState(id, StateDownloading) {
		return // evicted while waiting
	}
	s.report(&typesv1.ModelState{ModelId: id, State: StateDownloading})
	s.publish(id, StateDownloading, "")
	s.log().Info("mesh assignment: fetching and loading", "model", id)

	_, err := s.Ops.LoadInstanceOrigin(ctx, id, models.OriginMesh)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		if !s.stillActive(id) {
			return // evicted mid-download; already reported
		}
		switch {
		case errors.Is(err, models.ErrOverBudget):
			s.conclude(id, StateDeclined, "does not fit in max_disk_mb")
		default:
			s.conclude(id, StateFailed, err.Error())
		}
		return
	}
	s.mu.Lock()
	evicted := s.pending[id] != nil && s.pending[id].State == StateEvicted
	delete(s.pending, id)
	s.mu.Unlock()
	if evicted {
		// The coordinator withdrew it while the download ran: honour that
		// rather than announce a replica nobody wants.
		if err := s.Ops.Unload(ctx, id); err != nil {
			s.log().Warn("post-evict unload failed", "model", id, "err", err)
		}
		_ = s.Mgr.Remove(id)
		return
	}
	s.report(&typesv1.ModelState{ModelId: id, State: StateReady})
	s.publish(id, StateReady, "")
	s.log().Info("mesh assignment ready", "model", id)
}

// conclude records a terminal failure/refusal, reports it once, and keeps
// it visible locally for failedRetention.
func (s *Service) conclude(id, state, reason string) {
	s.mu.Lock()
	if s.pending == nil {
		s.pending = map[string]*Pending{}
	}
	s.pending[id] = &Pending{ID: id, State: state, Since: time.Now(), Error: reason}
	s.queue = slices.DeleteFunc(s.queue, func(q string) bool { return q == id })
	s.mu.Unlock()
	s.log().Warn("mesh assignment not applied", "model", id, "state", state, "reason", reason)
	s.report(&typesv1.ModelState{ModelId: id, State: state})
	s.publish(id, state, reason)
}

// States returns the in-flight assignments for Hello/heartbeat model lists
// (assigned + downloading only; failures/refusals are one-shot reports,
// otherwise the coordinator would read every heartbeat as a fresh one).
func (s *Service) States() []*typesv1.ModelState {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*typesv1.ModelState
	for _, p := range s.pending {
		if p.State == StateAssigned || p.State == StateDownloading {
			out = append(out, &typesv1.ModelState{ModelId: p.ID, State: p.State})
		}
	}
	return out
}

// Pending returns every tracked assignment (local API), sorted by id.
func (s *Service) Pending() []Pending {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Pending, 0, len(s.pending))
	for _, p := range s.pending {
		out = append(out, *p)
	}
	slices.SortFunc(out, func(a, b Pending) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	return out
}

// Get returns one tracked assignment.
func (s *Service) Get(id string) (Pending, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pending[id]
	if !ok {
		return Pending{}, false
	}
	return *p, true
}

// ---- internals ----

func (s *Service) kick() {
	s.mu.Lock()
	if s.wake == nil {
		s.wake = make(chan struct{}, 1)
	}
	w := s.wake
	s.mu.Unlock()
	select {
	case w <- struct{}{}:
	default:
	}
}

func (s *Service) next() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return "", false
	}
	id := s.queue[0]
	s.queue = s.queue[1:]
	return id, true
}

func (s *Service) stillQueued(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pending[id]
	return ok && p.State == StateAssigned
}

func (s *Service) stillActive(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pending[id]
	return ok && (p.State == StateAssigned || p.State == StateDownloading)
}

func (s *Service) setState(id, state string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pending[id]
	if !ok || p.State == StateEvicted {
		return false
	}
	p.State = state
	p.Since = time.Now()
	return true
}

// expire forgets concluded records after failedRetention.
func (s *Service) expire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, p := range s.pending {
		switch p.State {
		case StateFailed, StateDeclined, StateEvicted:
			if time.Since(p.Since) > failedRetention {
				delete(s.pending, id)
			}
		}
	}
}

// pinned checks both the config pin list and the live cache pin flag.
func (s *Service) pinned(id string) bool {
	if slices.Contains(s.Policy().Pinned, id) {
		return true
	}
	for _, i := range s.Mgr.List() {
		if i.ID == id && i.Pinned {
			return true
		}
	}
	return false
}

func (s *Service) loaded(id string) bool {
	if s.Eng == nil {
		return false
	}
	for _, m := range s.Eng.Models() {
		if m.Spec.ID == id {
			return true
		}
	}
	return false
}

func (s *Service) report(m *typesv1.ModelState) {
	s.mu.Lock()
	fn := s.reporter
	s.mu.Unlock()
	if fn == nil {
		return
	}
	if err := fn(m); err != nil {
		s.log().Debug("model state report dropped", "model", m.GetModelId(), "state", m.GetState(), "err", err)
	}
}

func (s *Service) publish(id, state, reason string) {
	if s.Events == nil {
		return
	}
	s.Events.Publish("model_assignment", map[string]string{"model": id, "state": state, "error": reason})
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Describe renders a pending record for logs/UI.
func (p Pending) Describe() string {
	if p.Error != "" {
		return fmt.Sprintf("%s (%s)", p.State, p.Error)
	}
	return p.State
}
