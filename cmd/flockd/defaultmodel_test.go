package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestRetryUntilLoadedKeepsTryingWithBackoff(t *testing.T) {
	var delays []time.Duration
	sleep := func(_ context.Context, d time.Duration) bool { delays = append(delays, d); return true }
	calls := 0
	load := func(context.Context) error {
		calls++
		if calls < 4 {
			return errors.New("download: no route to host")
		}
		return nil
	}
	if err := retryUntilLoaded(context.Background(), "m", load, slog.New(slog.NewTextHandler(io.Discard, nil)), sleep); err != nil {
		t.Fatal(err)
	}
	if calls != 4 {
		t.Fatalf("calls = %d, want 4", calls)
	}
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second}
	if len(delays) != len(want) {
		t.Fatalf("delays = %v, want %v", delays, want)
	}
	for i := range want {
		if delays[i] != want[i] {
			t.Fatalf("delays = %v, want %v", delays, want)
		}
	}
}

func TestRetryUntilLoadedCapsTheDelayAndStopsWithTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var last time.Duration
	n := 0
	sleep := func(_ context.Context, d time.Duration) bool {
		last = d
		n++
		if n == 12 {
			cancel()
			return false
		}
		return true
	}
	err := retryUntilLoaded(ctx, "m", func(context.Context) error { return errors.New("still no") }, slog.New(slog.NewTextHandler(io.Discard, nil)), sleep)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if last != defaultLoadRetryCap {
		t.Fatalf("delay after %d failures = %v, want the cap %v", n, last, defaultLoadRetryCap)
	}
	// A load that fails because the context ended is not retried.
	calls := 0
	err = retryUntilLoaded(ctx, "m", func(context.Context) error { calls++; return ctx.Err() }, slog.New(slog.NewTextHandler(io.Discard, nil)), sleep)
	if calls != 1 || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ctx: calls=%d err=%v", calls, err)
	}
}

func TestSleepCtxReturnsEarlyWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepCtx(ctx, time.Hour) {
		t.Fatal("slept through a cancelled context")
	}
	if !sleepCtx(context.Background(), time.Millisecond) {
		t.Fatal("did not sleep")
	}
}
