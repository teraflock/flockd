package llamacpp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	rt "github.com/hivegrid/hived/internal/runtime"
)

// Adapter implements runtime.Runtime by supervising llama-server and
// translating CompletionRequests to its OpenAI-compatible HTTP API.
type Adapter struct {
	Fetcher *Fetcher
	Accel   string // preferred accelerator backend from hardware detection
	Log     *slog.Logger
	// ContextLength override (0 = model/catalog default).
	ContextLength int
}

func (a *Adapter) logger() *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return slog.Default()
}

// Load fetches/verifies the pinned llama-server build, launches it on an
// ephemeral loopback port with the model, health-gates it and returns the
// serving Instance.
func (a *Adapter) Load(ctx context.Context, m rt.ModelSpec, res rt.ResourceBudget) (rt.Instance, error) {
	bin, buildID, err := a.Fetcher.Ensure(ctx, a.Accel)
	if err != nil {
		return nil, err
	}
	port, err := ephemeralPort()
	if err != nil {
		return nil, err
	}

	ctxLen := a.ContextLength
	if ctxLen == 0 {
		ctxLen = m.ContextLength
	}
	args := []string{
		"-m", m.Path,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(port),
		"--parallel", strconv.Itoa(max(res.MaxConcurrent, 1)),
		"--no-webui",
	}
	if ctxLen > 0 {
		args = append(args, "--ctx-size", strconv.Itoa(ctxLen))
	}
	if m.Embeddings {
		args = append(args, "--embeddings")
	}

	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	sup := newSupervisor(bin, args, url, a.logger().With("model", m.ID, "runtime_build", buildID))
	if err := sup.start(ctx); err != nil {
		return nil, err
	}
	return &instance{
		spec:    m,
		buildID: buildID,
		sup:     sup,
		baseURL: url,
		client:  &http.Client{}, // no timeout: streams are long-lived; ctx governs
	}, nil
}

type instance struct {
	spec    rt.ModelSpec
	buildID string
	sup     *supervisor
	baseURL string
	client  *http.Client
}

func (i *instance) Complete(ctx context.Context, req rt.CompletionRequest) (rt.TokenStream, error) {
	switch req.Kind {
	case rt.KindEmbedding:
		return i.embed(ctx, req)
	case rt.KindChat, rt.KindCompletion:
		return i.generate(ctx, req)
	default:
		return nil, fmt.Errorf("llamacpp: unsupported request kind %d", req.Kind)
	}
}

// ---- OpenAI-compatible wire shapes (subset llama-server implements) ----

type oaChatRequest struct {
	Model            string       `json:"model"`
	Messages         []rt.Message `json:"messages,omitempty"`
	Prompt           string       `json:"prompt,omitempty"`
	Stream           bool         `json:"stream"`
	Seed             *uint64      `json:"seed,omitempty"`
	Temperature      *float64     `json:"temperature,omitempty"`
	TopP             *float64     `json:"top_p,omitempty"`
	MaxTokens        int          `json:"max_tokens,omitempty"`
	Stop             []string     `json:"stop,omitempty"`
	FrequencyPenalty *float64     `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64     `json:"presence_penalty,omitempty"`
	StreamOptions    *oaStreamOpt `json:"stream_options,omitempty"`
}

type oaStreamOpt struct {
	IncludeUsage bool `json:"include_usage"`
}

type oaStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		Text         string  `json:"text"` // /v1/completions stream
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func (i *instance) generate(ctx context.Context, req rt.CompletionRequest) (rt.TokenStream, error) {
	body := oaChatRequest{
		Model:     i.spec.ID,
		Stream:    true,
		Seed:      &req.Params.Seed,
		MaxTokens: req.Params.MaxTokens,
		Stop:      req.Params.Stop,
		StreamOptions: &oaStreamOpt{
			IncludeUsage: true,
		},
	}
	body.Temperature = &req.Params.Temperature
	if req.Params.TopP > 0 {
		body.TopP = &req.Params.TopP
	}
	if req.Params.FrequencyPenalty != 0 {
		body.FrequencyPenalty = &req.Params.FrequencyPenalty
	}
	if req.Params.PresencePenalty != 0 {
		body.PresencePenalty = &req.Params.PresencePenalty
	}

	path := "/v1/chat/completions"
	if req.Kind == rt.KindCompletion {
		path = "/v1/completions"
		body.Prompt = req.Prompt
	} else {
		body.Messages = req.Messages
	}

	genCtx, cancel := context.WithCancel(ctx)
	resp, err := i.post(genCtx, path, body)
	if err != nil {
		cancel()
		return nil, err
	}

	ch := make(chan rt.Chunk, 16)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		parseSSE(genCtx, resp, ch)
	}()
	return rt.NewChanStream(ch, cancel), nil
}

// parseSSE reads llama-server's OpenAI-style SSE stream into Chunks.
func parseSSE(ctx context.Context, resp *http.Response, ch chan<- rt.Chunk) {
	var usage *rt.Usage
	finish := ""
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var c oaStreamChunk
		if err := json.Unmarshal([]byte(payload), &c); err != nil {
			send(ctx, ch, rt.Chunk{Done: true, FinishReason: "error", Err: fmt.Sprintf("llamacpp: bad stream chunk: %v", err)})
			return
		}
		if c.Usage != nil {
			usage = &rt.Usage{PromptTokens: c.Usage.PromptTokens, CompletionTokens: c.Usage.CompletionTokens}
		}
		for _, choice := range c.Choices {
			delta := choice.Delta.Content
			if delta == "" {
				delta = choice.Text
			}
			if delta != "" {
				if !send(ctx, ch, rt.Chunk{Delta: delta, TokenCount: 1}) {
					return
				}
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finish = *choice.FinishReason
			}
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		send(ctx, ch, rt.Chunk{Done: true, FinishReason: "error", Err: fmt.Sprintf("llamacpp: stream read: %v", err)})
		return
	}
	if ctx.Err() != nil {
		finish = "cancelled"
	} else if finish == "" {
		finish = "stop"
	}
	send(ctx, ch, rt.Chunk{Done: true, FinishReason: finish, Usage: usage})
}

func send(ctx context.Context, ch chan<- rt.Chunk, c rt.Chunk) bool {
	select {
	case ch <- c:
		return true
	case <-ctx.Done():
		return false
	}
}

type oaEmbeddingRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type oaEmbeddingResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
	} `json:"usage"`
}

func (i *instance) embed(ctx context.Context, req rt.CompletionRequest) (rt.TokenStream, error) {
	resp, err := i.post(ctx, "/v1/embeddings", oaEmbeddingRequest{Model: i.spec.ID, Input: req.EmbeddingInput})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var er oaEmbeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		return nil, fmt.Errorf("llamacpp: decode embeddings: %w", err)
	}
	vecs := make([][]float32, len(er.Data))
	for n, d := range er.Data {
		vecs[n] = d.Embedding
	}
	ch := make(chan rt.Chunk, 1)
	ch <- rt.Chunk{Done: true, Embeddings: vecs, Usage: &rt.Usage{PromptTokens: er.Usage.PromptTokens}}
	close(ch)
	return rt.NewChanStream(ch, nil), nil
}

func (i *instance) post(ctx context.Context, path string, body any) (*http.Response, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("llamacpp: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, i.baseURL+path, bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("llamacpp: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := i.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("llamacpp: %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		var msg bytes.Buffer
		_, _ = msg.ReadFrom(bufio.NewReaderSize(resp.Body, 4096))
		return nil, fmt.Errorf("llamacpp: %s: status %s: %s", path, resp.Status, strings.TrimSpace(msg.String()))
	}
	return resp, nil
}

type llamaHealthProps struct {
	SlotsIdle       int `json:"slots_idle"`
	SlotsProcessing int `json:"slots_processing"`
}

func (i *instance) Health(ctx context.Context) (rt.Stats, error) {
	st := rt.Stats{ModelID: i.spec.ID, Restarts: int(i.sup.restarts.Load())}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, i.baseURL+"/health", nil)
	if err != nil {
		return st, err
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return st, nil // unhealthy, not an error: supervisor is on it
	}
	defer resp.Body.Close()
	st.Healthy = resp.StatusCode == http.StatusOK
	var hp llamaHealthProps
	if json.NewDecoder(resp.Body).Decode(&hp) == nil {
		st.QueueDepth = hp.SlotsProcessing
	}
	return st, nil
}

func (i *instance) Shutdown(ctx context.Context) error {
	i.sup.stop(ctx)
	return nil
}

// RuntimeBuildID implements runtime.BuildIdentified: the pinned llama.cpp
// build this instance is supervising, as published by hivegrid/runtimes.
func (i *instance) RuntimeBuildID() string { return i.buildID }
