// Package runtime defines the inference Runtime interface (SPEC §A1.3) and
// its adapters. llama.cpp is supervised as a subprocess (never cgo); the
// mock runtime backs tests and --standalone demos.
package runtime

import (
	"context"
	"errors"
	"io"
)

// ModelSpec identifies a locally available model artifact ready to load.
// It is derived from the catalog's typesv1.ModelSpec after download+verify.
type ModelSpec struct {
	ID            string
	Family        string
	Quant         string
	SHA256        string
	Path          string // local GGUF path (part 1 of a sharded model)
	ContextLength int
	Embeddings    bool // model is served for /v1/embeddings
	// Decision marks a typed decision model served for /v1/systemone
	// (KindDecision): it answers bounded questions with probabilities and
	// generates no tokens. Mutually exclusive with Embeddings.
	Decision bool
	// MmprojPath is the vision projector sidecar passed to the runtime
	// (llama-server --mmproj); "" when the model has none.
	MmprojPath string
	// SizeBytes is what the artifact occupies on disk across all of its
	// files; 0 = unknown (the runtime stats Path instead).
	SizeBytes int64
}

// ResourceBudget is the operator-configured ceiling passed to Load, plus
// the layout admission planned for this particular load.
type ResourceBudget struct {
	MaxVRAMPercent int
	MaxRAMMB       int64
	// MaxConcurrent is the slot ceiling (budget.max_concurrent, or the
	// hardware default when that is 0).
	MaxConcurrent int
	// Slots and ContextTokens are what memory.PlanContext decided for
	// this load: --parallel and --ctx-size. 0 = unplanned; the adapter
	// falls back to MaxConcurrent and its own context resolution.
	Slots         int
	ContextTokens int
}

// Runtime loads models into serving Instances. Exactly per SPEC §A1.3.
type Runtime interface {
	Load(ctx context.Context, m ModelSpec, res ResourceBudget) (Instance, error)
}

// Instance is one loaded model.
type Instance interface {
	Complete(ctx context.Context, req CompletionRequest) (TokenStream, error)
	Health(ctx context.Context) (Stats, error)
	Shutdown(ctx context.Context) error
}

// Kind mirrors typesv1.RequestKind for runtime-local use.
type Kind int

const (
	KindChat Kind = iota + 1
	KindCompletion
	KindEmbedding
	KindDecision
)

// DecisionType is the type of one decision question (and of its answer);
// it mirrors typesv1.DecisionQuestionType.
type DecisionType int

const (
	DecisionChoice DecisionType = iota + 1 // pick one labelled option
	DecisionScore                          // place on an ordered scale
	DecisionNoul                           // probability a statement is true
)

// String is the public name of the type ("choice", "score", "noul").
func (t DecisionType) String() string {
	switch t {
	case DecisionChoice:
		return "choice"
	case DecisionScore:
		return "score"
	case DecisionNoul:
		return "noul"
	default:
		return "unknown"
	}
}

// DecisionOption is one possible answer. Order is part of the contract:
// the model reads the options in the order the customer wrote them.
type DecisionOption struct {
	// Key is the option key (choice), the level index "0", "1", … (score)
	// or "true"/"false" (noul, only when described).
	Key string
	// DescriptionJSON is compact JSON (string, object or array); "" means
	// JSON null: the option has no description.
	DescriptionJSON string
}

// DecisionQuestion is one typed question. Options is a list, never a
// map, so the order survives every hop.
type DecisionQuestion struct {
	ID               string
	Type             DecisionType
	InstructionsJSON string // compact JSON: string, object or array
	Options          []DecisionOption
}

// DecisionInput is a validated /v1/systemone request: the state and the
// questions in request order (ids unique).
type DecisionInput struct {
	StateJSON string // compact JSON: string, object or array
	Questions []DecisionQuestion
}

// DecisionProbability is the probability of one option (or score level).
type DecisionProbability struct {
	Key         string
	Probability float64
}

// DecisionAnswer is the typed answer to one question. Probabilities are
// ordered like the question's options and empty for noul.
type DecisionAnswer struct {
	QuestionID    string
	Type          DecisionType
	Choice        string  // choice: the most probable option key
	Score         float64 // score: expected level index
	Noul          float64 // noul: probability the statement is true
	Probabilities []DecisionProbability
	Confidence    float64 // choice and score: 0..1
}

// InvalidInputError means the runtime rejected the input itself (too many
// options for this model, a prompt over its context — llama-server's 400):
// another node would reject it too, so the request must not be retried
// elsewhere. Callers test with errors.As.
type InvalidInputError struct{ Msg string }

func (e *InvalidInputError) Error() string { return e.Msg }

// IsInvalidInput reports whether err is (or wraps) an InvalidInputError.
func IsInvalidInput(err error) bool {
	var ie *InvalidInputError
	return errors.As(err, &ie)
}

// ErrRuntimeTooOld is returned when a model needs a runtime build newer
// than the one this node runs (a decision model on a llama.cpp build that
// predates /v1/systemone).
var ErrRuntimeTooOld = errors.New("runtime: build too old for this model")

// ModelSupporter is implemented by runtimes that can tell, without
// loading, that their build cannot serve a model. modelops and assign ask
// before downloading so an unservable placement is refused up front.
type ModelSupporter interface {
	SupportsModel(ctx context.Context, m ModelSpec) error
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// GenerationParams: seed is always set upstream so retries and canary
// comparisons are deterministic for greedy decoding (SPEC §6).
type GenerationParams struct {
	Seed             uint64
	Temperature      float64
	TopP             float64
	MaxTokens        int
	Stop             []string
	FrequencyPenalty float64
	PresencePenalty  float64
}

type CompletionRequest struct {
	ID             string
	Model          string // requested model id ("" = node default)
	Kind           Kind
	Messages       []Message // KindChat
	Prompt         string    // KindCompletion
	EmbeddingInput []string  // KindEmbedding
	// Decision is the KindDecision payload. The result is a single final
	// Chunk carrying Decision answers (in question order) and Usage.
	Decision *DecisionInput
	Params   GenerationParams
	// Origin says who asked, for the live activity view only: "local"
	// (the local API; also what "" means), "mesh" (a coordinator
	// dispatch) or "challenge" (a fingerprint probe). The runtime
	// ignores it.
	Origin string
}

// Chunk is one streamed unit. For KindEmbedding a single final chunk
// carries Embeddings; for KindDecision a single final chunk carries
// Decision. Usage is set on the final chunk.
//
// Every token the model produces is relayed as either Delta (the answer)
// or Reasoning (chain-of-thought, for models whose runtime separates it),
// and TokenCount covers both: the customer is billed for reasoning tokens
// and canary comparison diffs the whole stream, so nothing is dropped on
// the node. A chunk may carry both when the runtime emits them together.
type Chunk struct {
	Delta string
	// Reasoning is a chain-of-thought delta (OpenAI's `reasoning_content`).
	// Empty for models the runtime cannot parse, whose thinking then
	// arrives inline in Delta (e.g. as `<think>…</think>`).
	Reasoning    string
	TokenCount   int
	Done         bool
	FinishReason string // "stop", "length", "cancelled", "error"
	Usage        *Usage
	Embeddings   [][]float32
	Decision     []DecisionAnswer // KindDecision: one per question, in order
	Err          string
}

type Usage struct {
	PromptTokens     int
	CompletionTokens int
}

// TokenStream yields Chunks until io.EOF. Close releases resources early
// (cancels generation server-side where supported).
type TokenStream interface {
	Recv() (Chunk, error)
	Close() error
}

// Stats is the Health snapshot.
type Stats struct {
	Healthy      bool
	ModelID      string
	QueueDepth   int
	TokensPerSec float64
	MemUsedMB    int64
	Restarts     int
}

// ErrNotLoaded is returned when an Instance is used after Shutdown or
// before the backing process is ready.
var ErrNotLoaded = errors.New("runtime: model not loaded")

// chanStream adapts a channel to TokenStream.
type chanStream struct {
	ch     <-chan Chunk
	cancel context.CancelFunc
}

// NewChanStream builds a TokenStream from a channel; cancel (may be nil) is
// invoked on Close.
func NewChanStream(ch <-chan Chunk, cancel context.CancelFunc) TokenStream {
	return &chanStream{ch: ch, cancel: cancel}
}

func (s *chanStream) Recv() (Chunk, error) {
	c, ok := <-s.ch
	if !ok {
		return Chunk{}, io.EOF
	}
	return c, nil
}

func (s *chanStream) Close() error {
	if s.cancel != nil {
		s.cancel()
	}
	return nil
}

// Drain reads a stream to completion and concatenates deltas. Convenience
// for non-streaming callers (challenges, tests). Reasoning deltas are
// discarded here; callers that must relay them use DrainAll.
func Drain(ts TokenStream) (text string, usage Usage, finish string, err error) {
	text, _, usage, finish, err = DrainAll(ts)
	return text, usage, finish, err
}

// DrainAll is Drain that also concatenates the reasoning deltas (localapi
// non-stream mode emits them as message.reasoning_content).
func DrainAll(ts TokenStream) (text, reasoning string, usage Usage, finish string, err error) {
	defer ts.Close()
	for {
		c, rerr := ts.Recv()
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return text, reasoning, usage, finish, nil
			}
			return text, reasoning, usage, finish, rerr
		}
		text += c.Delta
		reasoning += c.Reasoning
		if c.Usage != nil {
			usage = *c.Usage
		}
		if c.FinishReason != "" {
			finish = c.FinishReason
		}
		if c.Err != "" {
			return text, reasoning, usage, finish, errors.New(c.Err)
		}
	}
}

// MockRuntimeBuildID identifies the in-process mock as a runtime build. It
// is deliberately distinct from any real llama.cpp build id so that
// fingerprint expected outputs generated against a mock can never be
// mistaken for a reference for real hardware (SPEC §2.2).
const MockRuntimeBuildID = "mock-runtime-v1"

// BuildIdentified is implemented by runtime instances that know which
// runtime build they are running. The daemon reports this in its
// CapabilityProfile; it is one third of the fingerprint trust tuple
// (model_sha, quant, runtime_build_id) — SPEC §2.2, §A2.5.
type BuildIdentified interface {
	RuntimeBuildID() string
}
