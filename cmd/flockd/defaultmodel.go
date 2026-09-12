package main

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// bootState is what the local API reports while the node has nothing
// loaded: whether a default model is still on its way (flockd#48).
type bootState struct {
	pending atomic.Bool
	model   string
}

// DefaultPending reports whether the default model is still being
// fetched or loaded.
func (b *bootState) DefaultPending() bool { return b != nil && b.pending.Load() }

// Backoff for the default-model loop: quick first retries (a laptop that
// just woke, a captive portal being clicked through), then a gentle
// steady state that never gives up — the operator may add disk or
// network hours later, and a running daemon should notice.
const (
	defaultLoadRetryFirst = 5 * time.Second
	defaultLoadRetryCap = 5 * time.Minute
)

// retryUntilLoaded calls load until it succeeds or ctx ends, sleeping
// with doubling backoff between failures. Each failure is a warning with
// the reason and the next attempt's delay: a node with no model is a
// normal, reportable state, not a crash (flockd#48). sleep is injectable
// for tests.
func retryUntilLoaded(ctx context.Context, model string, load func(context.Context) error, log *slog.Logger, sleep func(context.Context, time.Duration) bool) error {
	delay := defaultLoadRetryFirst
	for attempt := 1; ; attempt++ {
		err := load(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		log.Warn("default model not loaded yet; will retry", "model", model, "attempt", attempt, "retry_in", delay, "err", err)
		if !sleep(ctx, delay) {
			return ctx.Err()
		}
		delay = min(delay*2, defaultLoadRetryCap)
	}
}

// sleepCtx waits d or until ctx ends; false when ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
