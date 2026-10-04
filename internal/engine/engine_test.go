package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
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

// A decision model serves decisions only: chat, completion and embedding
// requests are refused before admission and the runtime, with the
// gateway's wording, and leave nothing in flight.
func TestDecisionModelRefusesGeneration(t *testing.T) {
	e := New(nil, nil, nil)
	mock := rt.NewMockRuntime(0)
	for _, spec := range []rt.ModelSpec{{ID: "chat"}, {ID: "laya", Decision: true}} {
		inst, err := mock.Load(context.Background(), spec, rt.ResourceBudget{})
		if err != nil {
			t.Fatal(err)
		}
		e.Register(spec, inst)
	}
	for _, kind := range []rt.Kind{rt.KindChat, rt.KindCompletion, rt.KindEmbedding} {
		_, err := e.Complete(context.Background(), rt.CompletionRequest{Model: "laya", Kind: kind, Prompt: "hi", EmbeddingInput: []string{"a"}})
		if !errors.Is(err, ErrDecisionModel) || err.Error() != `model "laya" is a decision model: use POST /v1/systemone` {
			t.Fatalf("kind %d: err = %v", kind, err)
		}
	}
	if u, _ := e.Usage("laya"); u.Inflight != 0 {
		t.Fatalf("refused requests left %d in flight", u.Inflight)
	}
	if _, err := e.UnregisterIdle("laya"); err != nil {
		t.Fatalf("model is not idle after refusals: %v", err)
	}
}

func loadMock(t *testing.T, e *Engine, spec rt.ModelSpec) {
	t.Helper()
	inst, err := rt.NewMockRuntime(0).Load(context.Background(), spec, rt.ResourceBudget{})
	if err != nil {
		t.Fatal(err)
	}
	e.Register(spec, inst)
}

// Only a chat model can be the default: it answers chat requests that
// name no model. A decision or embedding model never becomes it — not by
// being loaded first, not when the default is unloaded, not on request.
func TestDefaultIsAlwaysAChatModel(t *testing.T) {
	e := New(nil, nil, nil)
	loadMock(t, e, rt.ModelSpec{ID: "kev-4b-q4_k_m", Decision: true})
	loadMock(t, e, rt.ModelSpec{ID: "nomic-embed", Embeddings: true})
	if d := e.DefaultModel(); d != "" {
		t.Fatalf("default = %q with only decision and embedding models loaded", d)
	}
	// A request that names no model has nothing to fall back on.
	_, err := e.Complete(context.Background(), rt.CompletionRequest{Kind: rt.KindChat})
	if !errors.Is(err, ErrModelNotFound) || !strings.Contains(err.Error(), "no default chat model is loaded") {
		t.Fatalf("chat with no default: %v", err)
	}

	loadMock(t, e, rt.ModelSpec{ID: "chat-a"})
	time.Sleep(2 * time.Millisecond)
	loadMock(t, e, rt.ModelSpec{ID: "chat-b"})
	if d := e.DefaultModel(); d != "chat-a" {
		t.Fatalf("default = %q, want the first chat model", d)
	}

	for _, id := range []string{"kev-4b-q4_k_m", "nomic-embed"} {
		err := e.SetDefault(id)
		if !errors.Is(err, ErrNotChatModel) {
			t.Fatalf("SetDefault(%s) = %v, want ErrNotChatModel", id, err)
		}
	}
	if err := e.SetDefault("kev-4b-q4_k_m"); !strings.Contains(err.Error(), "is a decision model") {
		t.Fatalf("message = %v", err)
	}
	if d := e.DefaultModel(); d != "chat-a" {
		t.Fatalf("a refused SetDefault changed the default to %q", d)
	}

	// Unloading the default hands over to the other chat model, then to
	// nothing: the decision model that is still loaded is not a candidate.
	e.Unregister("chat-a")
	if d := e.DefaultModel(); d != "chat-b" {
		t.Fatalf("default after unload = %q, want chat-b", d)
	}
	if _, err := e.UnregisterIdle("chat-b"); err != nil {
		t.Fatal(err)
	}
	if d := e.DefaultModel(); d != "" {
		t.Fatalf("default = %q with no chat model loaded", d)
	}
	// A chat model loaded later becomes the default again.
	loadMock(t, e, rt.ModelSpec{ID: "chat-c"})
	if d := e.DefaultModel(); d != "chat-c" {
		t.Fatalf("default = %q, want chat-c", d)
	}
}
