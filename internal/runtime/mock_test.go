package runtime

import (
	"context"
	"errors"
	"fmt"
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

func TestMockReasoningTokens(t *testing.T) {
	m := &MockRuntime{ReasoningTokens: 3}
	inst, err := m.Load(context.Background(), ModelSpec{ID: "m"}, ResourceBudget{})
	if err != nil {
		t.Fatal(err)
	}
	ts, err := inst.Complete(context.Background(), CompletionRequest{
		Kind: KindChat, Messages: []Message{{Role: "user", Content: "hi"}},
		Params: GenerationParams{Seed: 3, MaxTokens: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	text, reasoning, usage, _, err := DrainAll(ts)
	if err != nil {
		t.Fatal(err)
	}
	if reasoning == "" || text == "" {
		t.Fatalf("reasoning=%q text=%q: want both", reasoning, text)
	}
	if usage.CompletionTokens != 10 {
		t.Fatalf("completion_tokens = %d, want 10 (reasoning tokens billed)", usage.CompletionTokens)
	}
	// Raw completions have no chat template: no reasoning split.
	ts, err = inst.Complete(context.Background(), CompletionRequest{Kind: KindCompletion, Prompt: "x", Params: GenerationParams{Seed: 3, MaxTokens: 5}})
	if err != nil {
		t.Fatal(err)
	}
	if _, reasoning, _, _, _ = DrainAll(ts); reasoning != "" {
		t.Fatalf("completion produced reasoning %q", reasoning)
	}
}

func mockDecisionInput() *DecisionInput {
	return &DecisionInput{
		StateJSON: `"payouts failing for 3 days"`,
		Questions: []DecisionQuestion{
			{ID: "team", Type: DecisionChoice, InstructionsJSON: `"Which team?"`,
				Options: []DecisionOption{{Key: "technical"}, {Key: "billing", DescriptionJSON: `"Payments"`}, {Key: "account"}}},
			{ID: "urgency", Type: DecisionScore, InstructionsJSON: `"How urgent?"`,
				Options: []DecisionOption{{Key: "0", DescriptionJSON: `"can wait"`}, {Key: "1", DescriptionJSON: `"today"`}, {Key: "2", DescriptionJSON: `"now"`}}},
			{ID: "escalate", Type: DecisionNoul, InstructionsJSON: `"Escalate?"`},
		},
	}
}

func mockDecide(t *testing.T, inst Instance, in *DecisionInput) Chunk {
	t.Helper()
	ts, err := inst.Complete(context.Background(), CompletionRequest{Kind: KindDecision, Decision: in})
	if err != nil {
		t.Fatal(err)
	}
	c, err := ts.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Recv(); err != io.EOF {
		t.Fatalf("second Recv err = %v, want EOF", err)
	}
	return c
}

func TestMockDecision(t *testing.T) {
	inst, err := NewMockRuntime(0).Load(context.Background(), ModelSpec{ID: "mock-decision", Decision: true}, ResourceBudget{})
	if err != nil {
		t.Fatal(err)
	}
	in := mockDecisionInput()
	c := mockDecide(t, inst, in)
	if !c.Done || c.Usage == nil || c.Usage.PromptTokens == 0 || c.Usage.CompletionTokens != 0 {
		t.Fatalf("chunk = %+v usage = %+v", c, c.Usage)
	}
	if len(c.Decision) != 3 {
		t.Fatalf("answers = %d", len(c.Decision))
	}
	for n, q := range in.Questions {
		a := c.Decision[n]
		if a.QuestionID != q.ID || a.Type != q.Type {
			t.Fatalf("answer %d = %+v, want question %q", n, a, q.ID)
		}
		if q.Type == DecisionNoul {
			if a.Noul < 0 || a.Noul > 1 || a.Probabilities != nil {
				t.Fatalf("noul answer = %+v", a)
			}
			continue
		}
		sum, best := 0.0, 0
		for i, p := range a.Probabilities {
			if p.Key != q.Options[i].Key {
				t.Fatalf("%s: probability %d is for %q, want option order (%q)", q.ID, i, p.Key, q.Options[i].Key)
			}
			sum += p.Probability
			if p.Probability > a.Probabilities[best].Probability {
				best = i
			}
		}
		if sum < 0.999999 || sum > 1.000001 {
			t.Fatalf("%s: probabilities sum to %v", q.ID, sum)
		}
		if a.Confidence < 0 || a.Confidence > 1 {
			t.Fatalf("%s: confidence %v", q.ID, a.Confidence)
		}
		if q.Type == DecisionChoice && a.Choice != q.Options[best].Key {
			t.Fatalf("choice = %q, most probable is %q", a.Choice, q.Options[best].Key)
		}
		if q.Type == DecisionScore && (a.Score < 0 || a.Score > 2 || a.Choice != "") {
			t.Fatalf("score answer = %+v", a)
		}
	}

	// Deterministic: the same input gives the same answers (fingerprints).
	again := mockDecide(t, inst, mockDecisionInput())
	if fmt.Sprint(again.Decision) != fmt.Sprint(c.Decision) {
		t.Fatalf("mock decision not deterministic:\n%+v\n%+v", c.Decision, again.Decision)
	}
	// A different state gives different answers.
	other := mockDecisionInput()
	other.StateJSON = `"all good, thanks"`
	if fmt.Sprint(mockDecide(t, inst, other).Decision) == fmt.Sprint(c.Decision) {
		t.Fatal("answers do not depend on the state")
	}
}

func TestMockDecisionErrors(t *testing.T) {
	ctx := context.Background()
	dec, _ := NewMockRuntime(0).Load(ctx, ModelSpec{ID: "mock-decision", Decision: true}, ResourceBudget{})
	chat, _ := NewMockRuntime(0).Load(ctx, ModelSpec{ID: "mock-8b"}, ResourceBudget{})

	// Input the runtime itself rejects: InvalidInputError.
	if _, err := dec.Complete(ctx, CompletionRequest{Kind: KindDecision}); !IsInvalidInput(err) {
		t.Fatalf("nil input: err = %v", err)
	}
	tooMany := &DecisionInput{StateJSON: `"s"`, Questions: []DecisionQuestion{{ID: "a", Type: DecisionChoice, InstructionsJSON: `"q"`}}}
	for i := range 256 {
		tooMany.Questions[0].Options = append(tooMany.Questions[0].Options, DecisionOption{Key: fmt.Sprint(i)})
	}
	if _, err := dec.Complete(ctx, CompletionRequest{Kind: KindDecision, Decision: tooMany}); !IsInvalidInput(err) {
		t.Fatalf("256 options: err = %v", err)
	}
	// A chat model does not answer decisions, and a decision model does
	// not chat; neither is the customer's invalid input.
	if _, err := chat.Complete(ctx, CompletionRequest{Kind: KindDecision, Decision: mockDecisionInput()}); err == nil || IsInvalidInput(err) {
		t.Fatalf("decision on a chat model: err = %v", err)
	}
	if _, err := dec.Complete(ctx, CompletionRequest{Kind: KindChat, Messages: []Message{{Role: "user", Content: "hi"}}}); err == nil {
		t.Fatal("chat on a decision model succeeded")
	}
}
