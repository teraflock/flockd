// Package clock abstracts time for deterministic tests (governor yield
// latency, backoff, rolling stats).
package clock

import (
	"sync"
	"time"
)

// Clock is the minimal surface the daemon needs.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// Real is the wall clock.
type Real struct{}

func (Real) Now() time.Time                         { return time.Now() }
func (Real) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Fake is a manually advanced clock for tests.
type Fake struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*fakeWaiter
}

type fakeWaiter struct {
	at time.Time
	ch chan time.Time
}

// NewFake starts at a fixed, arbitrary epoch.
func NewFake() *Fake {
	return &Fake{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

// NewFakeAt starts at t.
func NewFakeAt(t time.Time) *Fake { return &Fake{now: t} }

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *Fake) After(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := &fakeWaiter{at: f.now.Add(d), ch: make(chan time.Time, 1)}
	if d <= 0 {
		w.ch <- f.now
		return w.ch
	}
	f.waiters = append(f.waiters, w)
	return w.ch
}

// Advance moves time forward and fires due waiters.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	now := f.now
	var due []*fakeWaiter
	keep := f.waiters[:0]
	for _, w := range f.waiters {
		if !w.at.After(now) {
			due = append(due, w)
		} else {
			keep = append(keep, w)
		}
	}
	f.waiters = keep
	f.mu.Unlock()
	for _, w := range due {
		w.ch <- now
	}
}

// Set jumps the clock to an absolute time and fires due waiters.
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	d := t.Sub(f.now)
	f.mu.Unlock()
	if d > 0 {
		f.Advance(d)
	}
}

// WaiterCount reports pending After() waiters (test synchronization).
func (f *Fake) WaiterCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.waiters)
}

// BlockUntilWaiters polls (real time) until at least n waiters are pending
// or the timeout elapses; returns whether the condition was met.
func (f *Fake) BlockUntilWaiters(n int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if f.WaiterCount() >= n {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return f.WaiterCount() >= n
}
