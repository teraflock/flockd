// Package vllm implements a flock runtime adapter that proxies to an
// already-running vLLM process. vLLM lifecycle management is external;
// this adapter only connects to it and forwards OpenAI-compatible requests.
package vllm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	rt "github.com/teraflock/flockd/internal/runtime"
)

// Adapter connects to an already-running vLLM server.
// Set BaseURL to the server root (e.g. "http://localhost:8000").
// If Model is empty, the first model from /v1/models is used.
type Adapter struct {
	BaseURL string
	Model   string
}

// Load health-gates the running vLLM instance and returns a serving Instance.
// ModelSpec and ResourceBudget are ignored — vLLM manages its own model.
func (a *Adapter) Load(ctx context.Context, _ rt.ModelSpec, _ rt.ResourceBudget) (rt.Instance, error) {
	model, err := a.resolveModel(ctx)
	if err != nil {
		return nil, fmt.Errorf("vllm: %w", err)
	}
	inst := &instance{
		baseURL: a.BaseURL,
		model:   model,
		client:  &http.Client{},
	}
	if err := inst.waitHealthy(ctx, 30*time.Second); err != nil {
		return nil, fmt.Errorf("vllm: %w", err)
	}
	return inst, nil
}

func (a *Adapter) resolveModel(ctx context.Context) (string, error) {
	if a.Model != "" {
		return a.Model, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.BaseURL+"/v1/models", nil)
	if err != nil {
		return "", fmt.Errorf("build /v1/models request: %w", err)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET /v1/models: %w", err)
	}
	defer resp.Body.Close()
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("decode /v1/models: %w", err)
	}
	if len(body.Data) == 0 {
		return "", fmt.Errorf("no models listed by vLLM at %s/v1/models", a.BaseURL)
	}
	return body.Data[0].ID, nil
}

// ---- instance ----

type instance struct {
	baseURL string
	model   string
	client  *http.Client
}

func (i *instance) RuntimeBuildID() string { return "vllm-proxy" }

func (i *instance) Complete(ctx context.Context, req rt.CompletionRequest) (rt.TokenStream, error) {
	if req.Kind == rt.KindEmbedding {
		return i.embed(ctx, req)
	}
	return i.generate(ctx, req)
}

// ---- OpenAI-compatible wire shapes ----

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
			Content   string `json:"content"`
			Reasoning string `json:"reasoning"` // reasoning models stream CoT here
		} `json:"delta"`
		Text         string  `json:"text"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func (i *instance) generate(ctx context.Context, req rt.CompletionRequest) (rt.TokenStream, error) {
	// vLLM validates seed as int64; mask the uint64 into range.
	seed := req.Params.Seed & 0x7FFFFFFFFFFFFFFF
	body := oaChatRequest{
		Model:  i.model,
		Stream: true,
		Seed:   &seed,
		StreamOptions: &oaStreamOpt{
			IncludeUsage: true,
		},
		MaxTokens: req.Params.MaxTokens,
		Stop:      req.Params.Stop,
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
			send(ctx, ch, rt.Chunk{Done: true, FinishReason: "error", Err: fmt.Sprintf("vllm: bad stream chunk: %v", err)})
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
			if delta == "" {
				delta = choice.Delta.Reasoning
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
		send(ctx, ch, rt.Chunk{Done: true, FinishReason: "error", Err: fmt.Sprintf("vllm: stream read: %v", err)})
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
	resp, err := i.post(ctx, "/v1/embeddings", oaEmbeddingRequest{Model: i.model, Input: req.EmbeddingInput})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var er oaEmbeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		return nil, fmt.Errorf("vllm: decode embeddings: %w", err)
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
		return nil, fmt.Errorf("vllm: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, i.baseURL+path, bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("vllm: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := i.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vllm: %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		var msg bytes.Buffer
		_, _ = msg.ReadFrom(bufio.NewReaderSize(resp.Body, 4096))
		return nil, fmt.Errorf("vllm: %s: status %s: %s", path, resp.Status, strings.TrimSpace(msg.String()))
	}
	return resp, nil
}

func (i *instance) Health(ctx context.Context) (rt.Stats, error) {
	st := rt.Stats{ModelID: i.model}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, i.baseURL+"/health", nil)
	if err != nil {
		return st, err
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return st, nil
	}
	defer resp.Body.Close()
	st.Healthy = resp.StatusCode == http.StatusOK
	return st, nil
}

func (i *instance) waitHealthy(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		h, _ := i.Health(ctx)
		if h.Healthy {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("server at %s not healthy after %s", i.baseURL, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (i *instance) Shutdown(_ context.Context) error {
	return nil // vLLM is externally managed; we do not stop it
}
