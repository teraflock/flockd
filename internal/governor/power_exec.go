//go:build darwin || linux

package governor

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"sync"
)

// execInhibitor holds the assertion for as long as a child process lives.
// Shared by macOS (caffeinate) and Linux (systemd-inhibit): Hold starts
// the child, Release cancels its context, which kills it and drops the
// assertion. One child at a time; its exit is reaped in the background
// so a Release never blocks on it longer than the kill takes.
type execInhibitor struct {
	log   *slog.Logger
	spawn func(ctx context.Context, reason string) *exec.Cmd

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

func (e *execInhibitor) Hold(reason string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cancel != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := e.spawn(ctx, reason)
	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("governor: %s: %w", cmd.Path, err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := cmd.Wait(); err != nil && ctx.Err() == nil && e.log != nil {
			e.log.Warn("keep-awake helper exited", "cmd", cmd.Path, "err", err)
		}
	}()
	e.cancel, e.done = cancel, done
	return nil
}

func (e *execInhibitor) Release() error {
	e.mu.Lock()
	cancel, done := e.cancel, e.done
	e.cancel, e.done = nil, nil
	e.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	<-done
	return nil
}

// Running reports whether the helper child is alive (tests).
func (e *execInhibitor) Running() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.done == nil {
		return false
	}
	select {
	case <-e.done:
		return false
	default:
		return true
	}
}
