package runtime

import (
	"context"
	"fmt"
	"hash/fnv"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// MockRuntime is a deterministic in-process runtime used by tests, the fake
// coordinator path, and `flockd --standalone --runtime=mock`. Same seed ->
// same output, so it also exercises the fingerprint-challenge flow.
type MockRuntime struct {
	// TokensPerSec throttles generation; <= 0 means unthrottled.
	TokensPerSec float64
	// FailLoads causes the next N Load calls to fail (supervisor tests).
	FailLoads int
	// ReasoningTokens makes chat generations emit this many chain-of-thought
	// tokens (Chunk.Reasoning) before the answer, the way llama.cpp's
	// reasoning_content parsing does for reasoning models. They count
	// toward max_tokens and completion_tokens like any other token.
	ReasoningTokens int
}

// NewMockRuntime returns a mock producing ~tps tokens/second.
func NewMockRuntime(tps float64) *MockRuntime {
	return &MockRuntime{TokensPerSec: tps}
}

func (m *MockRuntime) Load(_ context.Context, spec ModelSpec, res ResourceBudget) (Instance, error) {
	if m.FailLoads > 0 {
		m.FailLoads--
		return nil, fmt.Errorf("mock: simulated load failure for %s", spec.ID)
	}
	return &mockInstance{
		spec:      spec,
		tps:       m.TokensPerSec,
		maxConc:   res.MaxConcurrent,
		reasoning: m.ReasoningTokens,
	}, nil
}

type mockInstance struct {
	spec      ModelSpec
	tps       float64
	maxConc   int
	reasoning int

	mu       sync.Mutex
	shutdown bool

	inflight atomic.Int64
	tokTimes tokenWindow
}

var mockWords = strings.Fields(`the swarm hums quietly beneath the desk idle silicon wakes to
serve a stranger prompt tokens flow like nectar back through the tunnel
each cell of the grid earns its keep small models good enough five times
cheaper the flock remembers nothing it only computes and yields the moment
you touch the keyboard`)

func (i *mockInstance) Complete(ctx context.Context, req CompletionRequest) (TokenStream, error) {
	i.mu.Lock()
	if i.shutdown {
		i.mu.Unlock()
		return nil, ErrNotLoaded
	}
	i.mu.Unlock()

	if req.Kind == KindDecision {
		return i.decide(req)
	}
	if i.spec.Decision {
		// A decision model generates nothing (llama-server answers a chat
		// request on one with an error too).
		return nil, fmt.Errorf("mock: %s is a decision model: it serves /v1/systemone only", i.spec.ID)
	}
	if req.Kind == KindEmbedding {
		return i.embed(req), nil
	}

	genCtx, cancel := context.WithCancel(ctx)
	ch := make(chan Chunk, 8)
	i.inflight.Add(1)

	go func() {
		defer close(ch)
		defer i.inflight.Add(-1)

		rng := rand.New(rand.NewSource(int64(req.Params.Seed))) //nolint:gosec // deterministic by design
		maxTokens := req.Params.MaxTokens
		if maxTokens <= 0 {
			maxTokens = 64
		}
		promptTokens := estimateTokens(promptText(req))

		var interval time.Duration
		if i.tps > 0 {
			interval = time.Duration(float64(time.Second) / i.tps)
		}

		n := 0
		finish := "length"
		for ; n < maxTokens; n++ {
			if interval > 0 {
				select {
				case <-genCtx.Done():
					finish = "cancelled"
					goto done
				case <-time.After(interval):
				}
			} else if genCtx.Err() != nil {
				finish = "cancelled"
				break
			}
			word := mockWords[rng.Intn(len(mockWords))]
			chunk := Chunk{Delta: word + " ", TokenCount: 1}
			// Chain-of-thought first, then the answer (chat only: raw
			// completions have no template for the runtime to parse).
			thinking := req.Kind == KindChat && n < i.reasoning
			if thinking {
				chunk = Chunk{Reasoning: word + " ", TokenCount: 1}
			}
			i.tokTimes.record(time.Now())
			select {
			case ch <- chunk:
			case <-genCtx.Done():
				finish = "cancelled"
				goto done
			}
			// Deterministic early stop ~ mimics EOS.
			if !thinking && n >= 8 && rng.Float64() < 0.02 {
				n++
				finish = "stop"
				break
			}
		}
	done:
		ch <- Chunk{
			Done:         true,
			FinishReason: finish,
			Usage:        &Usage{PromptTokens: promptTokens, CompletionTokens: n},
		}
	}()

	return NewChanStream(ch, cancel), nil
}

func (*mockInstance) embed(req CompletionRequest) TokenStream {
	const dims = 64
	vecs := make([][]float32, len(req.EmbeddingInput))
	total := 0
	for n, in := range req.EmbeddingInput {
		h := fnv.New64a()
		_, _ = h.Write([]byte(in))
		rng := rand.New(rand.NewSource(int64(h.Sum64()))) //nolint:gosec
		v := make([]float32, dims)
		for d := range v {
			v[d] = rng.Float32()*2 - 1
		}
		vecs[n] = v
		total += estimateTokens(in)
	}
	ch := make(chan Chunk, 1)
	ch <- Chunk{
		Done:       true,
		Embeddings: vecs,
		Usage:      &Usage{PromptTokens: total},
	}
	close(ch)
	return NewChanStream(ch, nil)
}

// mockMaxChoiceOptions is the mock's stand-in for a model's own option
// limit (52 for openjev, 255 for laya): more than this is rejected as
// invalid input, the way llama-server answers 400.
const mockMaxChoiceOptions = 255

// decide answers a decision request deterministically: the probabilities
// are a pure function of the state, the question and its options, so two
// mock nodes agree (fingerprint and canary flows) and tests can pin
// values. No tokens are generated.
func (i *mockInstance) decide(req CompletionRequest) (TokenStream, error) {
	if !i.spec.Decision {
		return nil, fmt.Errorf("mock: %s is not a decision model", i.spec.ID)
	}
	in := req.Decision
	if in == nil || len(in.Questions) == 0 {
		return nil, &InvalidInputError{Msg: "mock: decision request has no questions"}
	}
	answers := make([]DecisionAnswer, 0, len(in.Questions))
	total := estimateTokens(in.StateJSON)
	for _, q := range in.Questions {
		h := fnv.New64a()
		_, _ = h.Write([]byte(in.StateJSON))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(q.ID))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(q.InstructionsJSON))
		total += estimateTokens(q.InstructionsJSON)
		for _, o := range q.Options {
			_, _ = h.Write([]byte{0})
			_, _ = h.Write([]byte(o.Key))
			total += estimateTokens(o.Key) + estimateTokens(o.DescriptionJSON)
		}
		rng := rand.New(rand.NewSource(int64(h.Sum64()))) //nolint:gosec // deterministic by design
		a := DecisionAnswer{QuestionID: q.ID, Type: q.Type}
		switch q.Type {
		case DecisionNoul:
			a.Noul = rng.Float64()
		case DecisionChoice, DecisionScore:
			if len(q.Options) == 0 {
				return nil, &InvalidInputError{Msg: fmt.Sprintf("mock: question %q has no options", q.ID)}
			}
			if len(q.Options) > mockMaxChoiceOptions {
				return nil, &InvalidInputError{Msg: fmt.Sprintf("mock: question %q has %d options, the model takes at most %d", q.ID, len(q.Options), mockMaxChoiceOptions)}
			}
			weights := make([]float64, len(q.Options))
			sum := 0.0
			for n := range weights {
				w := rng.Float64() + 0.01
				weights[n] = w * w // spread the mass a little
				sum += weights[n]
			}
			best := 0
			for n, o := range q.Options {
				p := weights[n] / sum
				a.Probabilities = append(a.Probabilities, DecisionProbability{Key: o.Key, Probability: p})
				a.Score += float64(n) * p
				if p > a.Probabilities[best].Probability {
					best = n
				}
			}
			// 0 = all options equally likely, 1 = one option certain.
			if n := float64(len(q.Options)); n > 1 {
				a.Confidence = (a.Probabilities[best].Probability - 1/n) / (1 - 1/n)
			} else {
				a.Confidence = 1
			}
			if q.Type == DecisionChoice {
				a.Choice, a.Score = q.Options[best].Key, 0
			}
		default:
			return nil, &InvalidInputError{Msg: fmt.Sprintf("mock: question %q has an unknown type", q.ID)}
		}
		answers = append(answers, a)
	}
	ch := make(chan Chunk, 1)
	ch <- Chunk{Done: true, Decision: answers, Usage: &Usage{PromptTokens: total}}
	close(ch)
	return NewChanStream(ch, nil), nil
}

func (i *mockInstance) Health(context.Context) (Stats, error) {
	i.mu.Lock()
	down := i.shutdown
	i.mu.Unlock()
	return Stats{
		Healthy:      !down,
		ModelID:      i.spec.ID,
		QueueDepth:   int(i.inflight.Load()),
		TokensPerSec: i.tokTimes.rate(time.Now(), time.Minute),
		MemUsedMB:    512,
	}, nil
}

func (i *mockInstance) Shutdown(context.Context) error {
	i.mu.Lock()
	i.shutdown = true
	i.mu.Unlock()
	return nil
}

func promptText(req CompletionRequest) string {
	if req.Kind == KindCompletion {
		return req.Prompt
	}
	var b strings.Builder
	for _, m := range req.Messages {
		b.WriteString(m.Content)
		b.WriteByte('\n')
	}
	return b.String()
}

// estimateTokens is the classic chars/4 heuristic; good enough for the mock
// and for local usage accounting (real counts come from llama-server).
func estimateTokens(s string) int {
	n := len(s) / 4
	if n == 0 && len(s) > 0 {
		n = 1
	}
	return n
}

// tokenWindow is a tiny ring of token timestamps for rate reporting.
type tokenWindow struct {
	mu sync.Mutex
	ts []time.Time
}

func (w *tokenWindow) record(t time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ts = append(w.ts, t)
	if len(w.ts) > 4096 {
		w.ts = w.ts[len(w.ts)-2048:]
	}
}

func (w *tokenWindow) rate(now time.Time, window time.Duration) float64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	cutoff := now.Add(-window)
	n := 0
	for i := len(w.ts) - 1; i >= 0 && w.ts[i].After(cutoff); i-- {
		n++
	}
	return float64(n) / window.Seconds()
}

// RuntimeBuildID implements BuildIdentified.
func (*mockInstance) RuntimeBuildID() string { return MockRuntimeBuildID }
