// Package activity is the daemon's short operator-facing history: what
// happened to the model store and the runtimes, by whom (the mesh, the
// operator, or the daemon's own housekeeping), and why. A fixed-size ring
// in memory — the operator wants to see "the mesh started a 30 GB download
// at 14:02", not a durable audit log.
package activity

import (
	"sync"
	"time"

	"github.com/teraflock/flockd/internal/events"
)

// Kinds of activity (api/openapi.yaml ActivityEvent.kind).
const (
	KindDownloadStarted = "download_started"
	KindDownloaded      = "downloaded"
	KindDownloadFailed  = "download_failed"
	KindLoaded          = "loaded"
	KindUnloaded        = "unloaded"
	KindEvicted         = "evicted"
	KindDeclined        = "declined"
	KindMissing         = "missing"
	KindUpdateAvailable = "update_available"
	KindAssignment      = "assignment"
)

// Actors: who caused the event.
const (
	ActorMesh     = "mesh"
	ActorOperator = "operator"
	ActorDaemon   = "daemon"
)

// Event is one row of the feed.
type Event struct {
	Time    time.Time `json:"time"`
	Kind    string    `json:"kind"`
	Actor   string    `json:"actor"`
	Model   string    `json:"model,omitempty"`
	Message string    `json:"message"`
	Detail  string    `json:"detail,omitempty"`
}

// DefaultCapacity is how many events a Ring keeps.
const DefaultCapacity = 200

// Ring keeps the last N events, newest first on read. Safe for concurrent
// use; a nil *Ring accepts and drops events so producers need no nil
// checks.
type Ring struct {
	// Events, when set, receives each event as an `activity` SSE event.
	Events *events.Hub

	mu    sync.Mutex
	buf   []Event
	next  int
	full  bool
	clock func() time.Time
}

// New builds a ring of capacity n (DefaultCapacity when n <= 0).
func New(n int) *Ring {
	if n <= 0 {
		n = DefaultCapacity
	}
	return &Ring{buf: make([]Event, n), clock: time.Now}
}

// Add records an event, stamping Time when unset.
func (r *Ring) Add(e Event) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if e.Time.IsZero() {
		e.Time = r.clock()
	}
	r.buf[r.next] = e
	r.next = (r.next + 1) % len(r.buf)
	if r.next == 0 {
		r.full = true
	}
	hub := r.Events
	r.mu.Unlock()
	hub.Publish("activity", e)
}

// Record is Add with the common fields spelled out.
func (r *Ring) Record(kind, actor, model, message, detail string) {
	r.Add(Event{Kind: kind, Actor: actor, Model: model, Message: message, Detail: detail})
}

// List returns the events newest first.
func (r *Ring) List() []Event {
	if r == nil {
		return []Event{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.next
	if r.full {
		n = len(r.buf)
	}
	out := make([]Event, 0, n)
	for i := 1; i <= n; i++ {
		idx := (r.next - i + len(r.buf)) % len(r.buf)
		out = append(out, r.buf[idx])
	}
	return out
}
