// Package events is the daemon's in-process event bus: governor state
// changes, model download progress, and model lifecycle changes fan out to
// SSE subscribers (web dash, desktop app, TUI). Publishing never blocks —
// a slow consumer misses intermediate events and converges on the next
// status snapshot.
package events

import "sync"

// Event is one published occurrence. Data must be JSON-marshalable.
type Event struct {
	Type string
	Data any
}

// Hub fans events out to subscribers.
type Hub struct {
	mu   sync.Mutex
	subs map[int]chan Event
	next int
}

// NewHub builds an empty hub.
func NewHub() *Hub {
	return &Hub{subs: map[int]chan Event{}}
}

// Publish delivers to every subscriber, dropping when a buffer is full.
// Safe on a nil hub (publishers don't care whether anyone wired events).
func (h *Hub) Publish(typ string, data any) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ch := range h.subs {
		select {
		case ch <- Event{Type: typ, Data: data}:
		default:
		}
	}
}

// Subscribe registers a consumer; call the returned func to detach.
func (h *Hub) Subscribe() (<-chan Event, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.next
	h.next++
	ch := make(chan Event, 64)
	h.subs[id] = ch
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.subs, id)
	}
}
