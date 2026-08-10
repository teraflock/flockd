package localapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"time"

	"github.com/hivegrid/hived/internal/engine"
	"github.com/hivegrid/hived/internal/governor"
	rt "github.com/hivegrid/hived/internal/runtime"
)

// ---- OpenAI wire shapes (subset) ----

type oaError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code,omitempty"`
	} `json:"error"`
}

func writeOpenAIError(w http.ResponseWriter, status int, typ, msg string) {
	var e oaError
	e.Error.Message = msg
	e.Error.Type = typ
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(e)
}

// writeEngineError maps engine/governor errors to OpenAI-shaped responses.
func writeEngineError(w http.ResponseWriter, err error) {
	var nse governor.ErrNotServing
	switch {
	case errors.As(err, &nse):
		writeOpenAIError(w, http.StatusServiceUnavailable, "server_error",
			fmt.Sprintf("node is not serving right now (%s); retry shortly", nse.State))
	case errors.Is(err, engine.ErrModelNotFound):
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", err.Error())
	default:
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
	}
}

type oaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type oaChatRequest struct {
	Model            string      `json:"model"`
	Messages         []oaMessage `json:"messages"`
	Prompt           any         `json:"prompt"` // /v1/completions: string or [string]
	Stream           bool        `json:"stream"`
	MaxTokens        int         `json:"max_tokens"`
	MaxCompletionTok int         `json:"max_completion_tokens"`
	Temperature      *float64    `json:"temperature"`
	TopP             *float64    `json:"top_p"`
	Seed             *uint64     `json:"seed"`
	Stop             any         `json:"stop"` // string or [string]
	FrequencyPenalty float64     `json:"frequency_penalty"`
	PresencePenalty  float64     `json:"presence_penalty"`
	StreamOptions    *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

func (r *oaChatRequest) params() rt.GenerationParams {
	p := rt.GenerationParams{
		Temperature:      0.8,
		MaxTokens:        r.MaxTokens,
		FrequencyPenalty: r.FrequencyPenalty,
		PresencePenalty:  r.PresencePenalty,
	}
	if r.MaxCompletionTok > 0 {
		p.MaxTokens = r.MaxCompletionTok
	}
	if p.MaxTokens <= 0 {
		p.MaxTokens = 256
	}
	if r.Temperature != nil {
		p.Temperature = *r.Temperature
	}
	if r.TopP != nil {
		p.TopP = *r.TopP
	}
	if r.Seed != nil {
		p.Seed = *r.Seed
	} else {
		// Seed is always set so dispatch retries and canary comparisons are
		// deterministic (SPEC §6).
		p.Seed = rand.Uint64() //nolint:gosec
	}
	switch s := r.Stop.(type) {
	case string:
		p.Stop = []string{s}
	case []any:
		for _, v := range s {
			if str, ok := v.(string); ok {
				p.Stop = append(p.Stop, str)
			}
		}
	}
	return p
}

type oaUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func usageOf(u rt.Usage) oaUsage {
	return oaUsage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.PromptTokens + u.CompletionTokens,
	}
}

func mapFinish(reason string) string {
	switch reason {
	case "length":
		return "length"
	case "cancelled", "error":
		return "stop" // OpenAI has no cancel finish; stream just ends
	default:
		return "stop"
	}
}

// ---- GET /v1/models ----

func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	out := struct {
		Object string  `json:"object"`
		Data   []model `json:"data"`
	}{Object: "list"}
	for _, m := range s.deps.Engine.Models() {
		out.Data = append(out.Data, model{ID: m.Spec.ID, Object: "model", Created: s.start.Unix(), OwnedBy: "hivegrid"})
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- POST /v1/chat/completions ----

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	var req oaChatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20)).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "messages is required")
		return
	}
	creq := rt.CompletionRequest{
		ID:     newRequestID(),
		Model:  req.Model,
		Kind:   rt.KindChat,
		Params: req.params(),
	}
	for _, m := range req.Messages {
		creq.Messages = append(creq.Messages, rt.Message{Role: m.Role, Content: m.Content})
	}
	s.serveGeneration(w, r, req, creq, "chat.completion")
}

// ---- POST /v1/completions ----

func (s *Server) handleCompletions(w http.ResponseWriter, r *http.Request) {
	var req oaChatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20)).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}
	prompt := ""
	switch p := req.Prompt.(type) {
	case string:
		prompt = p
	case []any:
		for _, v := range p {
			if str, ok := v.(string); ok {
				prompt += str
			}
		}
	}
	if prompt == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "prompt is required")
		return
	}
	creq := rt.CompletionRequest{
		ID:     newRequestID(),
		Model:  req.Model,
		Kind:   rt.KindCompletion,
		Prompt: prompt,
		Params: req.params(),
	}
	s.serveGeneration(w, r, req, creq, "text_completion")
}

// serveGeneration handles both streaming SSE and non-streaming modes.
func (s *Server) serveGeneration(w http.ResponseWriter, r *http.Request, oa oaChatRequest, creq rt.CompletionRequest, object string) {
	stream, err := s.deps.Engine.Complete(r.Context(), creq)
	if err != nil {
		writeEngineError(w, err)
		return
	}

	model := creq.Model
	if model == "" {
		model = s.deps.Engine.DefaultModel()
	}
	id := "chatcmpl-" + creq.ID
	created := time.Now().Unix()
	chat := object == "chat.completion"

	if !oa.Stream {
		defer stream.Close()
		text, usage, finish, derr := rt.Drain(stream)
		if derr != nil {
			writeEngineError(w, derr)
			return
		}
		resp := map[string]any{
			"id":      id,
			"object":  object,
			"created": created,
			"model":   model,
			"usage":   usageOf(usage),
		}
		if chat {
			resp["choices"] = []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": text},
				"finish_reason": mapFinish(finish),
			}}
		} else {
			resp["choices"] = []map[string]any{{
				"index":         0,
				"text":          text,
				"finish_reason": mapFinish(finish),
			}}
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// Streaming SSE.
	defer stream.Close()
	fl, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "streaming unsupported by connection")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	emit := func(payload any) {
		raw, _ := json.Marshal(payload)
		fmt.Fprintf(w, "data: %s\n\n", raw)
		fl.Flush()
	}

	sentRole := false
	streamObj := object
	if chat {
		streamObj = "chat.completion.chunk"
	}
	for {
		chunk, rerr := stream.Recv()
		if rerr != nil {
			break // EOF or transport error: finish with [DONE]
		}
		if chunk.Err != "" {
			// Emit an error frame the SDKs surface, then end.
			emit(map[string]any{"error": map[string]any{"message": chunk.Err, "type": "server_error"}})
			break
		}
		if chunk.Delta != "" {
			var choice map[string]any
			if chat {
				delta := map[string]any{"content": chunk.Delta}
				if !sentRole {
					delta["role"] = "assistant"
					sentRole = true
				}
				choice = map[string]any{"index": 0, "delta": delta, "finish_reason": nil}
			} else {
				choice = map[string]any{"index": 0, "text": chunk.Delta, "finish_reason": nil}
			}
			emit(map[string]any{
				"id": id, "object": streamObj, "created": created, "model": model,
				"choices": []map[string]any{choice},
			})
		}
		if chunk.Done {
			var choice map[string]any
			if chat {
				choice = map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": mapFinish(chunk.FinishReason)}
			} else {
				choice = map[string]any{"index": 0, "text": "", "finish_reason": mapFinish(chunk.FinishReason)}
			}
			final := map[string]any{
				"id": id, "object": streamObj, "created": created, "model": model,
				"choices": []map[string]any{choice},
			}
			if chunk.Usage != nil && (oa.StreamOptions == nil || oa.StreamOptions.IncludeUsage) {
				final["usage"] = usageOf(*chunk.Usage)
			}
			emit(final)
			break
		}
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	fl.Flush()
}

// ---- POST /v1/embeddings ----

func (s *Server) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string `json:"model"`
		Input any    `json:"input"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20)).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}
	var input []string
	switch v := req.Input.(type) {
	case string:
		input = []string{v}
	case []any:
		for _, item := range v {
			if str, ok := item.(string); ok {
				input = append(input, str)
			}
		}
	}
	if len(input) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "input is required")
		return
	}

	stream, err := s.deps.Engine.Complete(r.Context(), rt.CompletionRequest{
		ID:             newRequestID(),
		Model:          req.Model,
		Kind:           rt.KindEmbedding,
		EmbeddingInput: input,
	})
	if err != nil {
		writeEngineError(w, err)
		return
	}
	defer stream.Close()

	var vecs [][]float32
	var usage rt.Usage
	for {
		chunk, rerr := stream.Recv()
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			writeEngineError(w, rerr)
			return
		}
		vecs = append(vecs, chunk.Embeddings...)
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
		if chunk.Done {
			break
		}
	}

	model := req.Model
	if model == "" {
		model = s.deps.Engine.DefaultModel()
	}
	type embedding struct {
		Object    string    `json:"object"`
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	}
	out := struct {
		Object string      `json:"object"`
		Data   []embedding `json:"data"`
		Model  string      `json:"model"`
		Usage  oaUsage     `json:"usage"`
	}{Object: "list", Model: model, Usage: usageOf(usage)}
	for i, v := range vecs {
		out.Data = append(out.Data, embedding{Object: "embedding", Index: i, Embedding: v})
	}
	writeJSON(w, http.StatusOK, out)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func newRequestID() string {
	return fmt.Sprintf("%08x%08x", rand.Uint32(), rand.Uint32()) //nolint:gosec
}
