package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/teraflock/flockd/internal/governor"
	rt "github.com/teraflock/flockd/internal/runtime"
)

func newEngine(t *testing.T, admit Admitter) (*Engine, []string) {
	t.Helper()
	var touched []string
	e := New(admit, nil, func(id string) { touched = append(touched, id) })
	mock := rt.NewMockRuntime(0)
	for _, id := range []string{"model-a", "model-b"} {
		inst, err := mock.Load(context.Background(), rt.ModelSpec{ID: id}, rt.ResourceBudget{MaxConcurrent: 2})
		if err != nil {
			t.Fatal(err)
		}
		e.Register(rt.ModelSpec{ID: id}, inst)
	}
	return e, touched
}

func TestRoutesToRequestedAndDefaultModel(t *testing.T) {
	e, _ := newEngine(t, nil)
	if e.DefaultModel() != "model-a" {
		t.Fatalf("default = %q", e.DefaultModel())
	}
	// Explicit model.
	ts, err := e.Complete(context.Background(), rt.CompletionRequest{
		ID: "r1", Model: "model-b", Kind: rt.KindChat,
		Messages: []rt.Message{{Role: "user", Content: "x"}},
		Params:   rt.GenerationParams{Seed: 1, MaxTokens: 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := rt.Drain(ts); err != nil {
		t.Fatal(err)
	}
	// Empty model falls back to default.
	ts, err = e.Complete(context.Background(), rt.CompletionRequest{
		ID: "r2", Kind: rt.KindChat,
		Messages: []rt.Message{{Role: "user", Content: "x"}},
		Params:   rt.GenerationParams{Seed: 1, MaxTokens: 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, _ = rt.Drain(ts)
	// Unknown model errors.
	if _, err := e.Complete(context.Background(), rt.CompletionRequest{ID: "r3", Model: "nope", Kind: rt.KindChat}); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestMeteringAndPayout(t *testing.T) {
	e, _ := newEngine(t, nil)
	ts, err := e.Complete(context.Background(), rt.CompletionRequest{
		ID: "r1", Kind: rt.KindChat,
		Messages: []rt.Message{{Role: "user", Content: "hello"}},
		Params:   rt.GenerationParams{Seed: 9, MaxTokens: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, usage, _, err := rt.Drain(ts)
	if err != nil {
		t.Fatal(err)
	}
	snap := e.Stats().Snapshot()
	if snap.TotalRequests != 1 || snap.TotalTokens != int64(usage.CompletionTokens) {
		t.Errorf("snapshot = %+v, usage = %+v", snap, usage)
	}
	want := int64(usage.CompletionTokens) * payoutMicroPerToken
	if snap.EarnedMicrocred != want {
		t.Errorf("earned = %d, want %d", snap.EarnedMicrocred, want)
	}
	if snap.Inflight != 0 {
		t.Errorf("inflight = %d after drain", snap.Inflight)
	}
}

func TestAdmissionGating(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	gov := governor.New(governor.Policy{Serve: "idle-only", IdleAfter: time.Minute},
		&governor.FakeIdleSource{}, &governor.FakePowerSource{}, nil, quiet)
	e, _ := newEngine(t, gov) // starts yielded
	_, err := e.Complete(context.Background(), rt.CompletionRequest{
		ID: "r1", Kind: rt.KindChat, Messages: []rt.Message{{Role: "user", Content: "x"}},
	})
	var nse governor.ErrNotServing
	if !errors.As(err, &nse) {
		t.Fatalf("err = %v, want ErrNotServing", err)
	}
	if gov.Inflight() != 0 {
		t.Error("rejected request must not leak inflight")
	}
}

func TestTouchAndUnregister(t *testing.T) {
	e, _ := newEngine(t, nil)
	e2 := e // touched slice captured by closure in newEngine; re-verify via lookup
	entry := e2.Unregister("model-a")
	if entry == nil || e2.DefaultModel() != "model-b" {
		t.Fatalf("unregister: default = %q", e2.DefaultModel())
	}
	if _, err := e2.Complete(context.Background(), rt.CompletionRequest{ID: "r", Model: "model-a", Kind: rt.KindChat}); err == nil {
		t.Fatal("unregistered model still serving")
	}
}

// gateAdmitter blocks every Admit until released, and signals when a
// request is waiting inside it.
type gateAdmitter struct {
	waiting chan struct{}
	release chan struct{}
}

func (g *gateAdmitter) Admit(ctx context.Context, _ string) (context.Context, func(), error) {
	g.waiting <- struct{}{}
	select {
	case <-g.release:
		return ctx, func() {}, nil
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

func TestUnregisterIdleRefusesRoutedRequests(t *testing.T) {
	gate := &gateAdmitter{waiting: make(chan struct{}, 1), release: make(chan struct{})}
	e, _ := newEngine(t, gate)
	req := rt.CompletionRequest{ID: "r", Model: "model-b", Kind: rt.KindChat,
		Messages: []rt.Message{{Role: "user", Content: "x"}}, Params: rt.GenerationParams{Seed: 1, MaxTokens: 4}}

	// A request routed to the model but still waiting in admission — the
	// window the old code left open — already counts as in flight.
	done := make(chan error, 1)
	go func() {
		ts, err := e.Complete(context.Background(), req)
		if err == nil {
			_, _, _, err = rt.Drain(ts)
		}
		done <- err
	}()
	<-gate.waiting
	if _, err := e.UnregisterIdle("model-b"); !errors.Is(err, ErrBusy) {
		t.Fatalf("UnregisterIdle during admission: err = %v, want ErrBusy", err)
	}
	if u, ok := e.Usage("model-b"); !ok || u.Inflight != 1 {
		t.Fatalf("usage during admission = %+v %v", u, ok)
	}
	close(gate.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if u, _ := e.Usage("model-b"); u.Inflight != 0 {
		t.Fatalf("inflight after drain = %d", u.Inflight)
	}
	entry, err := e.UnregisterIdle("model-b")
	if err != nil || entry == nil || entry.Spec.ID != "model-b" {
		t.Fatalf("UnregisterIdle when idle: %v %v", entry, err)
	}
	if _, err := e.UnregisterIdle("model-b"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("second UnregisterIdle: %v", err)
	}
	if _, err := e.Complete(context.Background(), req); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("request after unregister: %v", err)
	}
}

func TestAdmitFailureReleasesInflight(t *testing.T) {
	gate := &gateAdmitter{waiting: make(chan struct{}, 1), release: make(chan struct{})}
	e, _ := newEngine(t, gate)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := e.Complete(ctx, rt.CompletionRequest{ID: "r", Model: "model-a", Kind: rt.KindChat})
		done <- err
	}()
	<-gate.waiting
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if u, _ := e.Usage("model-a"); u.Inflight != 0 {
		t.Fatalf("rejected request leaked inflight: %d", u.Inflight)
	}
}
