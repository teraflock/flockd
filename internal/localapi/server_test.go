package localapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teraflock/flockd/internal/engine"
	"github.com/teraflock/flockd/internal/governor"
	rt "github.com/teraflock/flockd/internal/runtime"
	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
)

const testToken = "flock_testtoken"

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestServer(t *testing.T, gov *governor.Governor) *httptest.Server {
	t.Helper()
	eng := engine.New(gov, nil, nil)
	mock := rt.NewMockRuntime(0)
	inst, err := mock.Load(context.Background(), rt.ModelSpec{ID: "mock-8b-instruct"}, rt.ResourceBudget{MaxConcurrent: 4})
	if err != nil {
		t.Fatal(err)
	}
	eng.Register(rt.ModelSpec{ID: "mock-8b-instruct"}, inst)

	s := New(Deps{
		Engine:     eng,
		Governor:   gov,
		Hardware:   &typesv1.CapabilityProfile{Os: "darwin", Arch: "arm64", CpuCores: 8},
		Log:        quietLog(),
		NodeID:     "node-test",
		Version:    "test",
		Standalone: true,
		Token:      testToken,
	})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func servingGovernor(t *testing.T) *governor.Governor {
	t.Helper()
	idle := &governor.FakeIdleSource{}
	idle.Set(time.Hour)
	g := governor.New(governor.Policy{Serve: "always"}, idle, &governor.FakePowerSource{}, nil, quietLog())
	return g
}

func apiGet(t *testing.T, srv *httptest.Server, path string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestChatCompletionsNonStreaming(t *testing.T) {
	srv := newTestServer(t, servingGovernor(t))
	body := `{"model":"mock-8b-instruct","messages":[{"role":"user","content":"hi"}],"max_tokens":16,"seed":7}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Object != "chat.completion" || len(out.Choices) != 1 {
		t.Fatalf("out = %+v", out)
	}
	if out.Choices[0].Message.Content == "" || out.Choices[0].Message.Role != "assistant" {
		t.Errorf("message = %+v", out.Choices[0].Message)
	}
	if out.Usage.CompletionTokens == 0 || out.Usage.TotalTokens == 0 {
		t.Errorf("usage = %+v", out.Usage)
	}
}

func TestChatCompletionsStreaming(t *testing.T) {
	srv := newTestServer(t, servingGovernor(t))
	body := `{"model":"mock-8b-instruct","messages":[{"role":"user","content":"hi"}],"max_tokens":12,"stream":true,"seed":7}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	var deltas []string
	sawDone, sawFinish := false, false
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			sawDone = true
			break
		}
		var chunk struct {
			Object  string `json:"object"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("bad chunk %q: %v", payload, err)
		}
		if chunk.Object != "chat.completion.chunk" {
			t.Errorf("object = %q", chunk.Object)
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" {
				deltas = append(deltas, c.Delta.Content)
			}
			if c.FinishReason != nil && *c.FinishReason != "" {
				sawFinish = true
			}
		}
	}
	if len(deltas) < 2 {
		t.Errorf("streamed %d deltas, want several", len(deltas))
	}
	if !sawDone || !sawFinish {
		t.Errorf("sawDone=%v sawFinish=%v", sawDone, sawFinish)
	}
}

func TestCompletionsEndpoint(t *testing.T) {
	srv := newTestServer(t, servingGovernor(t))
	body := `{"model":"mock-8b-instruct","prompt":"Once upon","max_tokens":8,"seed":3}`
	resp, err := http.Post(srv.URL+"/v1/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Object  string `json:"object"`
		Choices []struct {
			Text string `json:"text"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Object != "text_completion" || len(out.Choices) != 1 || out.Choices[0].Text == "" {
		t.Fatalf("out = %+v", out)
	}
}

func TestEmbeddingsEndpoint(t *testing.T) {
	srv := newTestServer(t, servingGovernor(t))
	body := `{"model":"mock-8b-instruct","input":["hello","world"]}`
	resp, err := http.Post(srv.URL+"/v1/embeddings", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Object string `json:"object"`
		Data   []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 2 || len(out.Data[0].Embedding) == 0 || out.Data[1].Index != 1 {
		t.Fatalf("out = %+v", out)
	}
}

func TestModelsEndpoint(t *testing.T) {
	srv := newTestServer(t, servingGovernor(t))
	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 1 || out.Data[0].ID != "mock-8b-instruct" {
		t.Fatalf("models = %+v", out.Data)
	}
}

func TestYieldedNodeReturns503(t *testing.T) {
	idle := &governor.FakeIdleSource{} // idle 0 => active => yielded
	gov := governor.New(governor.Policy{Serve: "idle-only", IdleAfter: time.Minute},
		idle, &governor.FakePowerSource{}, nil, quietLog())
	srv := newTestServer(t, gov)

	body := `{"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	var e struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&e)
	if e.Error.Type != "server_error" {
		t.Errorf("error = %+v", e)
	}
}

func TestUnknownModel404(t *testing.T) {
	srv := newTestServer(t, servingGovernor(t))
	body := `{"model":"gpt-42","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestAPIRequiresBearerToken(t *testing.T) {
	srv := newTestServer(t, servingGovernor(t))
	resp, err := http.Get(srv.URL + "/api/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", resp.StatusCode)
	}

	resp = apiGet(t, srv, "/api/v1/status")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated status = %d", resp.StatusCode)
	}
	var st StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.NodeID != "node-test" || st.State != "serving" || st.DefaultModel != "mock-8b-instruct" {
		t.Errorf("status = %+v", st)
	}
}

func TestLimitsRoundTrip(t *testing.T) {
	srv := newTestServer(t, servingGovernor(t))
	resp := apiGet(t, srv, "/api/v1/limits")
	var lim Limits
	_ = json.NewDecoder(resp.Body).Decode(&lim)
	resp.Body.Close()
	if lim.ServePolicy != "always" {
		t.Fatalf("limits = %+v", lim)
	}

	lim.ServePolicy = "scheduled"
	lim.Schedule = []string{"22:00-08:00"}
	raw, _ := json.Marshal(lim)
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/v1/limits", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+testToken)
	putResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer putResp.Body.Close()
	if putResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(putResp.Body)
		t.Fatalf("put status = %d: %s", putResp.StatusCode, body)
	}
	var got Limits
	_ = json.NewDecoder(putResp.Body).Decode(&got)
	if got.ServePolicy != "scheduled" || len(got.Schedule) != 1 || got.Schedule[0] != "22:00-08:00" {
		t.Errorf("updated limits = %+v", got)
	}
}

func TestEarningsEndpoint(t *testing.T) {
	srv := newTestServer(t, servingGovernor(t))
	// Serve one request so earnings tick.
	body := `{"messages":[{"role":"user","content":"hi"}],"max_tokens":16,"seed":1}`
	r, _ := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	io.Copy(io.Discard, r.Body) //nolint:errcheck
	r.Body.Close()

	resp := apiGet(t, srv, "/api/v1/earnings")
	defer resp.Body.Close()
	var e EarningsResponse
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatal(err)
	}
	if e.EarnedMicrocredits <= 0 || e.LifetimeTokens <= 0 {
		t.Errorf("earnings = %+v", e)
	}
	if !strings.Contains(e.Note, "simulated") {
		t.Error("earnings must be honest about being simulated")
	}
}

func TestEventsSSETicker(t *testing.T) {
	srv := newTestServer(t, servingGovernor(t))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") {
			var st StatusResponse
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &st); err != nil {
				t.Fatalf("bad event payload: %v", err)
			}
			if st.NodeID != "node-test" {
				t.Errorf("event = %+v", st)
			}
			return // first event is enough
		}
	}
	t.Fatal("no SSE event received")
}

func TestTokenFileCreation(t *testing.T) {
	dir := t.TempDir()
	tok1, err := LoadOrCreateToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok1, "flock_") {
		t.Errorf("token = %q", tok1)
	}
	tok2, err := LoadOrCreateToken(dir)
	if err != nil || tok1 != tok2 {
		t.Errorf("token not stable: %q vs %q (%v)", tok1, tok2, err)
	}
}

// Operators paste this token by hand out of a file or a terminal, and a
// stray space is invisible in a password field. Rejecting it produced an
// "invalid token" the operator could not distinguish from a wrong token.
func TestBearerTokenToleratesSurroundingWhitespace(t *testing.T) {
	srv := newTestServer(t, servingGovernor(t))
	for _, header := range []string{
		"Bearer " + testToken,
		"Bearer  " + testToken,      // doubled space after the scheme
		"  Bearer " + testToken,     // leading whitespace
		"Bearer " + testToken + " ", // trailing whitespace
		"Bearer\t" + testToken,
		"bearer " + testToken, // RFC 6750: scheme is case-insensitive
		"BEARER " + testToken,
	} {
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/status", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", header)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("Authorization=%q -> %d, want 200", header, resp.StatusCode)
		}
	}
}

func TestBearerTokenStillRejectsWrongToken(t *testing.T) {
	srv := newTestServer(t, servingGovernor(t))
	for _, header := range []string{
		"",
		"Bearer ",
		"Bearer flock_wrong",
		"Bearer " + testToken + "x",
		"Basic " + testToken, // wrong scheme
		testToken,            // no scheme
	} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/status", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("Authorization=%q -> %d, want 401", header, resp.StatusCode)
		}
	}
}

// EventSource cannot set request headers, so the SSE route (and only it)
// accepts the token as a query parameter.
func TestEventsAcceptsQueryToken(t *testing.T) {
	srv := newTestServer(t, servingGovernor(t))

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/events?token="+testToken, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("events?token= -> %d, want 200", resp.StatusCode)
	}

	bad, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/events?token=nope", nil)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	resp2, err := http.DefaultClient.Do(bad.WithContext(ctx2))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("events?token=nope -> %d, want 401", resp2.StatusCode)
	}
}

// The query-parameter escape hatch must not leak to the rest of /api/v1,
// where it would end up in shell history and server logs for every call.
func TestQueryTokenRejectedOnNonSSERoutes(t *testing.T) {
	srv := newTestServer(t, servingGovernor(t))
	resp, err := http.Get(srv.URL + "/api/v1/status?token=" + testToken)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status?token= -> %d, want 401 (query tokens are SSE-only)", resp.StatusCode)
	}
}
