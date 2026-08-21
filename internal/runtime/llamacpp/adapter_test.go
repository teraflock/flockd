package llamacpp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	rt "github.com/teraflock/flockd/internal/runtime"
)

// fakeLlamaServer emulates the subset of llama-server's OpenAI-compatible
// API the adapter uses.
func fakeLlamaServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok","slots_idle":4,"slots_processing":1}`))
	})
	stream := func(w http.ResponseWriter, deltas []string, completionStyle bool) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, d := range deltas {
			if completionStyle {
				fmt.Fprintf(w, "data: {\"choices\":[{\"text\":%q,\"finish_reason\":null}]}\n\n", d)
			} else {
				fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", d)
			}
			fl.Flush()
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req oaChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !req.Stream {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if req.Seed == nil {
			http.Error(w, "seed must always be set", http.StatusBadRequest)
			return
		}
		stream(w, []string{"Hello", " from", " llama"}, false)
	})
	mux.HandleFunc("/v1/completions", func(w http.ResponseWriter, r *http.Request) {
		stream(w, []string{"one", " two"}, true)
	})
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2]},{"embedding":[0.3,0.4]}],"usage":{"prompt_tokens":5}}`))
	})
	return httptest.NewServer(mux)
}

func testInstance(t *testing.T, url string) *instance {
	t.Helper()
	return &instance{
		spec:    rt.ModelSpec{ID: "test-model"},
		sup:     newSupervisor("/bin/false", nil, url, slog.Default()),
		baseURL: url,
		client:  &http.Client{},
	}
}

func TestAdapterChatStreaming(t *testing.T) {
	srv := fakeLlamaServer(t)
	defer srv.Close()
	inst := testInstance(t, srv.URL)

	ts, err := inst.Complete(context.Background(), rt.CompletionRequest{
		Kind:     rt.KindChat,
		Messages: []rt.Message{{Role: "user", Content: "hi"}},
		Params:   rt.GenerationParams{Seed: 7, MaxTokens: 16},
	})
	if err != nil {
		t.Fatal(err)
	}
	text, usage, finish, err := rt.Drain(ts)
	if err != nil {
		t.Fatal(err)
	}
	if text != "Hello from llama" {
		t.Errorf("text = %q", text)
	}
	if finish != "stop" || usage.PromptTokens != 7 || usage.CompletionTokens != 3 {
		t.Errorf("finish=%q usage=%+v", finish, usage)
	}
}

func TestAdapterCompletionStreaming(t *testing.T) {
	srv := fakeLlamaServer(t)
	defer srv.Close()
	inst := testInstance(t, srv.URL)

	ts, err := inst.Complete(context.Background(), rt.CompletionRequest{
		Kind:   rt.KindCompletion,
		Prompt: "count:",
		Params: rt.GenerationParams{Seed: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	text, _, _, err := rt.Drain(ts)
	if err != nil || text != "one two" {
		t.Errorf("text = %q, err = %v", text, err)
	}
}

func TestAdapterEmbeddings(t *testing.T) {
	srv := fakeLlamaServer(t)
	defer srv.Close()
	inst := testInstance(t, srv.URL)

	ts, err := inst.Complete(context.Background(), rt.CompletionRequest{
		Kind:           rt.KindEmbedding,
		EmbeddingInput: []string{"a", "b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := ts.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Embeddings) != 2 || c.Embeddings[1][1] != 0.4 {
		t.Errorf("embeddings = %v", c.Embeddings)
	}
	if c.Usage == nil || c.Usage.PromptTokens != 5 {
		t.Errorf("usage = %+v", c.Usage)
	}
}

func TestAdapterServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"model exploded"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()
	inst := testInstance(t, srv.URL)
	_, err := inst.Complete(context.Background(), rt.CompletionRequest{
		Kind:     rt.KindChat,
		Messages: []rt.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("want 500 error, got %v", err)
	}
}

func TestAdapterHealth(t *testing.T) {
	srv := fakeLlamaServer(t)
	defer srv.Close()
	inst := testInstance(t, srv.URL)
	st, err := inst.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Healthy || st.QueueDepth != 1 || st.ModelID != "test-model" {
		t.Errorf("stats = %+v", st)
	}
}
