package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// The OpenAI-compatible half (/v1) is an external wire standard, so the
// spec documents it without schemas and nothing is generated for it. This
// is the subset tera needs: the model list and chat completions, streamed
// or not, with the `reasoning_content` extension the daemon emits for
// reasoning models (internal/localapi/openai.go).

// Message is one chat turn.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatRequest is the request shape `scripts/smoke.sh` and the OpenAI SDKs
// send. Zero values are omitted so the daemon's defaults apply.
type ChatRequest struct {
	// Model is the model id; empty means the daemon's default model.
	Model       string    `json:"model,omitempty"`
	Messages    []Message `json:"messages"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`
	Seed        *uint64   `json:"seed,omitempty"`
}

// Delta is one streamed chunk: answer text and/or chain-of-thought.
type Delta struct {
	Content   string
	Reasoning string
}

// Usage is the OpenAI usage frame.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatResult is a finished completion: the assembled text, the usage
// frame, and how long it took (for tok/s).
type ChatResult struct {
	Model        string
	Content      string
	Reasoning    string
	FinishReason string
	Usage        Usage
	Elapsed      time.Duration
}

// TokensPerSec is completion tokens over wall time (0 when unknown).
func (r ChatResult) TokensPerSec() float64 {
	if r.Elapsed <= 0 || r.Usage.CompletionTokens == 0 {
		return 0
	}
	return float64(r.Usage.CompletionTokens) / r.Elapsed.Seconds()
}

// OpenAIModels is GET /v1/models: the ids of the models the daemon serves
// right now (loaded, plus what it can load on demand).
func (c *Client) OpenAIModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	_ = c.bearer(ctx, req)
	resp, err := doer{c.hc, c.Base}.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if err := c.Check(resp.StatusCode, raw); err != nil {
		return nil, err
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode /v1/models: %w", err)
	}
	ids := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		ids = append(ids, m.ID)
	}
	return ids, nil
}

// Chat is a non-streaming POST /v1/chat/completions. A governor refusal
// comes back as an *APIError with NotServing() true; Remedy(err) says
// what to do about it.
func (c *Client) Chat(ctx context.Context, req ChatRequest) (ChatResult, error) {
	start := time.Now()
	body := struct {
		ChatRequest
		Stream bool `json:"stream"`
	}{req, false}
	resp, err := c.postV1(ctx, c.long, "/v1/chat/completions", body)
	if err != nil {
		return ChatResult{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if err := c.Check(resp.StatusCode, raw); err != nil {
		return ChatResult{}, err
	}
	var out struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				Reasoning string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage Usage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return ChatResult{}, fmt.Errorf("decode chat completion: %w", err)
	}
	res := ChatResult{Model: out.Model, Usage: out.Usage, Elapsed: time.Since(start)}
	if len(out.Choices) > 0 {
		res.Content = out.Choices[0].Message.Content
		res.Reasoning = out.Choices[0].Message.Reasoning
		res.FinishReason = out.Choices[0].FinishReason
	}
	return res, nil
}

// ChatStream is a streaming POST /v1/chat/completions: fn is called for
// every delta as it arrives (returning an error stops the stream), and
// the result carries the assembled text plus the usage frame the daemon
// sends with the final chunk. An error frame mid-stream ends it with
// that message.
func (c *Client) ChatStream(ctx context.Context, req ChatRequest, fn func(Delta) error) (ChatResult, error) {
	start := time.Now()
	body := struct {
		ChatRequest
		Stream        bool `json:"stream"`
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}{ChatRequest: req, Stream: true}
	body.StreamOptions.IncludeUsage = true
	resp, err := c.postV1(ctx, c.long, "/v1/chat/completions", body)
	if err != nil {
		return ChatResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return ChatResult{}, c.Check(resp.StatusCode, raw)
	}

	var res ChatResult
	var content, reasoning strings.Builder
	var streamErr error
	rerr := ReadSSE(resp.Body, func(ev Event) error {
		data := bytes.TrimSpace(ev.Data)
		if string(data) == "[DONE]" {
			return ErrStop
		}
		var chunk struct {
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					Reasoning string `json:"reasoning_content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *Usage `json:"usage"`
			Error *struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal(data, &chunk); err != nil {
			return nil // tolerate frames we do not understand
		}
		if chunk.Error != nil {
			streamErr = &APIError{Status: http.StatusInternalServerError, Type: chunk.Error.Type, Message: chunk.Error.Message}
			return ErrStop
		}
		if chunk.Model != "" {
			res.Model = chunk.Model
		}
		if chunk.Usage != nil {
			res.Usage = *chunk.Usage
		}
		for _, ch := range chunk.Choices {
			d := Delta{Content: ch.Delta.Content, Reasoning: ch.Delta.Reasoning}
			content.WriteString(d.Content)
			reasoning.WriteString(d.Reasoning)
			if ch.FinishReason != nil && *ch.FinishReason != "" {
				res.FinishReason = *ch.FinishReason
			}
			if d.Content != "" || d.Reasoning != "" {
				if err := fn(d); err != nil {
					return err
				}
			}
		}
		return nil
	})
	res.Content, res.Reasoning, res.Elapsed = content.String(), reasoning.String(), time.Since(start)
	switch {
	case streamErr != nil:
		return res, streamErr
	case ctx.Err() != nil:
		return res, ctx.Err()
	case rerr != nil && !errors.Is(rerr, ErrStop):
		return res, rerr
	}
	return res, nil
}

func (c *Client) postV1(ctx context.Context, hc *http.Client, path string, body any) (*http.Response, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// Keyless on loopback by default; the token is harmless when
	// local_api.require_auth_v1 is off and required when it is on.
	_ = c.bearer(ctx, req)
	return doer{hc, c.Base}.Do(req)
}
