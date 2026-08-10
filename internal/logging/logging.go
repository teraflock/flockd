// Package logging provides slog setup plus an in-memory ring buffer handler
// backing GET /api/v1/logs. Request content is never logged (SPEC §2.1);
// only operational events flow here.
package logging

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

// Entry is one captured log record.
type Entry struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
	Attrs   string    `json:"attrs,omitempty"`
}

// Ring is a fixed-size record buffer. Safe for concurrent use.
type Ring struct {
	mu      sync.Mutex
	entries []Entry
	next    int
	full    bool
}

// NewRing allocates a buffer of n entries.
func NewRing(n int) *Ring {
	if n <= 0 {
		n = 512
	}
	return &Ring{entries: make([]Entry, n)}
}

func (r *Ring) add(e Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[r.next] = e
	r.next = (r.next + 1) % len(r.entries)
	if r.next == 0 {
		r.full = true
	}
}

// Tail returns up to n most recent entries, oldest first.
func (r *Ring) Tail(n int) []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ordered []Entry
	if r.full {
		ordered = append(ordered, r.entries[r.next:]...)
	}
	ordered = append(ordered, r.entries[:r.next]...)
	if n > 0 && len(ordered) > n {
		ordered = ordered[len(ordered)-n:]
	}
	out := make([]Entry, len(ordered))
	copy(out, ordered)
	return out
}

// ringHandler tees records into the ring.
type ringHandler struct {
	ring  *Ring
	inner slog.Handler
}

func (h *ringHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *ringHandler) Handle(ctx context.Context, rec slog.Record) error {
	var attrs []string
	rec.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, fmt.Sprintf("%s=%v", a.Key, a.Value))
		return true
	})
	h.ring.add(Entry{
		Time:    rec.Time,
		Level:   rec.Level.String(),
		Message: rec.Message,
		Attrs:   strings.Join(attrs, " "),
	})
	return h.inner.Handle(ctx, rec)
}

func (h *ringHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &ringHandler{ring: h.ring, inner: h.inner.WithAttrs(attrs)}
}

func (h *ringHandler) WithGroup(name string) slog.Handler {
	return &ringHandler{ring: h.ring, inner: h.inner.WithGroup(name)}
}

// New builds the daemon logger: level/format from config, teed into a Ring.
func New(level, format string) (*slog.Logger, *Ring) {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var inner slog.Handler
	if strings.ToLower(format) == "json" {
		inner = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		inner = slog.NewTextHandler(os.Stderr, opts)
	}
	ring := NewRing(1024)
	return slog.New(&ringHandler{ring: ring, inner: inner}), ring
}
