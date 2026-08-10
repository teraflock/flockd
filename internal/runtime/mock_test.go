package runtime

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func loadMock(t *testing.T, tps float64) Instance {
	t.Helper()
	rt := NewMockRuntime(tps)
	inst, err := rt.Load(context.Background(), ModelSpec{ID: "mock-8b"}, ResourceBudget{MaxConcurrent: 2})
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

func TestMockDeterministicBySeed(t *testing.T) {
	inst := loadMock(t, 0) // unthrottled

	run := func() string {
		ts, err := inst.Complete(context.Background(), CompletionRequest{
			Kind:     KindChat,
			Messages: []Message{{Role: "user", Content: "hello"}},
			Params:   GenerationParams{Seed: 42, MaxTokens: 32},
		})
		if err != nil {
			t.Fatal(err)
		}
		text, usage, finish, err := Drain(ts)
		if err != nil {
			t.Fatal(err)
		}
		if usage.CompletionTokens == 0 || finish == "" {
			t.Fatalf("usage=%+v finish=%q", usage, finish)
		}
		return text
	}

	a, b := run(), run()
	if a != b {
		t.Errorf("same seed produced different output:\n%q\n%q", a, b)
	}

	ts, _ := inst.Complete(context.Background(), CompletionRequest{
		Kind:     KindChat,
		Messages: []Message{{Role: "user", Content: "hello"}},
		Params:   GenerationParams{Seed: 43, MaxTokens: 32},
	})
	c, _, _, _ := Drain(ts)
	if c == a {
		t.Error("different seed produced identical output")
	}
}

func TestMockCancellation(t *testing.T) {
	inst := loadMock(t, 20) // slow: 50ms/token
	ctx, cancel := context.WithCancel(context.Background())
	ts, err := inst.Complete(ctx, CompletionRequest{
		Kind:   KindCompletion,
		Prompt: "long",
		Params: GenerationParams{Seed: 1, MaxTokens: 1000},
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(120 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, _, finish, err := Drain(ts)
	if err != nil {
		t.Fatal(err)
	}
	if finish != "cancelled" {
		t.Errorf("finish = %q, want cancelled", finish)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("cancellation took too long")
	}
}

func TestMockEmbeddingsDeterministic(t *testing.T) {
	inst := loadMock(t, 0)
	get := func() [][]float32 {
		ts, err := inst.Complete(context.Background(), CompletionRequest{
			Kind:           KindEmbedding,
			EmbeddingInput: []string{"alpha", "beta"},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer ts.Close()
		c, err := ts.Recv()
		if err != nil {
			t.Fatal(err)
		}
		return c.Embeddings
	}
	a, b := get(), get()
	if len(a) != 2 || len(a[0]) != 64 {
		t.Fatalf("shape = %dx%d", len(a), len(a[0]))
	}
	if a[0][0] != b[0][0] || a[1][3] != b[1][3] {
		t.Error("embeddings not deterministic")
	}
}

func TestMockShutdownRejects(t *testing.T) {
	inst := loadMock(t, 0)
	if err := inst.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := inst.Complete(context.Background(), CompletionRequest{Kind: KindChat})
	if !errors.Is(err, ErrNotLoaded) {
		t.Errorf("err = %v, want ErrNotLoaded", err)
	}
	st, err := inst.Health(context.Background())
	if err != nil || st.Healthy {
		t.Errorf("health after shutdown: %+v, %v", st, err)
	}
}

func TestDrainEOF(t *testing.T) {
	ch := make(chan Chunk, 2)
	ch <- Chunk{Delta: "a"}
	ch <- Chunk{Done: true, FinishReason: "stop", Usage: &Usage{CompletionTokens: 1}}
	close(ch)
	s := NewChanStream(ch, nil)
	text, usage, finish, err := Drain(s)
	if err != nil || text != "a" || finish != "stop" || usage.CompletionTokens != 1 {
		t.Errorf("got %q %+v %q %v", text, usage, finish, err)
	}
	if _, rerr := s.Recv(); !errors.Is(rerr, io.EOF) {
		t.Error("expected EOF after close")
	}
}
