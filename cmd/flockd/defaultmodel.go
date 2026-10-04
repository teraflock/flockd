package main

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/teraflock/flockd/internal/modelops"
	"github.com/teraflock/flockd/internal/models"
	rt "github.com/teraflock/flockd/internal/runtime"
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
	defaultLoadRetryCap   = 5 * time.Minute
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

// defaultFallback decides whether start-up should give up on the default
// model `want` and load `base` (the default as config.toml or the built-in
// default has it) instead. That is the case when want was chosen through
// the API (it differs from base) and can never work as the default: it is
// not in the catalog, its id is not valid, the runtime build cannot serve
// it, or it loaded but is not a chat model (notChat).
// Transient failures — no network, no memory yet — keep retrying want.
// It returns the model to load next and the reason, or "" to carry on.
func defaultFallback(want, base string, loadErr error, notChat bool) (next, why string) {
	if base == "" || base == want {
		return "", ""
	}
	switch {
	case loadErr == nil && notChat:
		return base, want + " is not a chat model"
	case errors.Is(loadErr, modelops.ErrUnknownModel):
		return base, want + " is not in the catalog"
	case errors.Is(loadErr, models.ErrInvalidID):
		return base, want + " is not a valid model id"
	case errors.Is(loadErr, rt.ErrRuntimeTooOld):
		return base, loadErr.Error()
	}
	return "", ""
}
