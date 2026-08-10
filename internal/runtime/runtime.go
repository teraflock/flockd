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
	Path          string // local GGUF path
	ContextLength int
	Embeddings    bool // model is served for /v1/embeddings
}

// ResourceBudget is the operator-configured ceiling passed to Load.
type ResourceBudget struct {
	MaxVRAMPercent int
	MaxRAMMB       int64
	MaxConcurrent  int
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
)

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
	Params         GenerationParams
}

// Chunk is one streamed unit. For KindEmbedding a single final chunk
// carries Embeddings. Usage is set on the final chunk.
type Chunk struct {
	Delta        string
	TokenCount   int
	Done         bool
	FinishReason string // "stop", "length", "cancelled", "error"
	Usage        *Usage
	Embeddings   [][]float32
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
// for non-streaming callers (localapi non-stream mode, challenges, tests).
func Drain(ts TokenStream) (text string, usage Usage, finish string, err error) {
	defer ts.Close()
	for {
		c, rerr := ts.Recv()
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return text, usage, finish, nil
			}
			return text, usage, finish, rerr
		}
		text += c.Delta
		if c.Usage != nil {
			usage = *c.Usage
		}
		if c.FinishReason != "" {
			finish = c.FinishReason
		}
		if c.Err != "" {
			return text, usage, finish, errors.New(c.Err)
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
