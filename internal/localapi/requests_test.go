package localapi

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teraflock/flockd/internal/engine"
	"github.com/teraflock/flockd/internal/events"
	"github.com/teraflock/flockd/internal/localapi/gen"
	rt "github.com/teraflock/flockd/internal/runtime"
)

// activityServer is a node with a slow chat model, a decision model and
// an event hub wired the way the daemon wires it.
func activityServer(t *testing.T) (*httptest.Server, *engine.Engine) {
	t.Helper()
	hub := events.NewHub()
	eng := engine.New(nil, nil, nil)
	eng.Requests().Events = hub
	slow, err := rt.NewMockRuntime(20).Load(context.Background(), rt.ModelSpec{ID: "chat-model"}, rt.ResourceBudget{})
	if err != nil {
		t.Fatal(err)
	}
	eng.Register(rt.ModelSpec{ID: "chat-model"}, slow)
	dec, err := rt.NewMockRuntime(0).Load(context.Background(), rt.ModelSpec{ID: "laya-q8_0", Quant: "Q8_0", Decision: true}, rt.ResourceBudget{})
	if err != nil {
		t.Fatal(err)
	}
	eng.Register(rt.ModelSpec{ID: "laya-q8_0", Quant: "Q8_0", Decision: true}, dec)
	s := New(Deps{Engine: eng, Events: hub, Log: quietLog(), NodeID: "node-test", Version: "test", Standalone: true, Token: testToken})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv, eng
}

func getRequests(t *testing.T, srv *httptest.Server, query string) (gen.RequestActivity, string) {
	t.Helper()
	resp := apiGet(t, srv, "/api/v1/requests"+query)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/requests%s = %d", query, resp.StatusCode)
	}
	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	var ra gen.RequestActivity
	if err := json.Unmarshal(raw, &ra); err != nil {
		t.Fatal(err)
	}
	return ra, string(raw)
}

func TestRequestsAPI(t *testing.T) {
	srv, eng := activityServer(t)

	// Idle node: both models listed, nothing in flight, empty arrays (not null).
	ra, raw := getRequests(t, srv, "")
	if len(ra.Models) != 2 || ra.Models[0].Model != "chat-model" || ra.Models[1].Model != "laya-q8_0" {
		t.Fatalf("models = %+v", ra.Models)
	}
	if ra.Models[0].Kind != gen.ModelActivityKindChat || ra.Models[1].Kind != gen.ModelActivityKindDecision {
		t.Fatalf("model kinds = %+v", ra.Models)
	}
	for _, m := range ra.Models {
		if m.Inflight != 0 || m.IdleSeconds == nil || m.LastRequestAt != nil || len(m.InflightByKind) != 0 {
			t.Fatalf("idle model = %+v", m)
		}
	}
	for _, want := range []string{`"inflight":[]`, `"recent":[]`, `"operations":[]`, `"inflight_by_kind":{}`, `"now":"`} {
		if !strings.Contains(raw, want) {
			t.Fatalf("response lacks %s: %s", want, raw)
		}
	}

	// A streaming chat held open: in flight, with tokens so far.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ts, err := eng.Complete(ctx, rt.CompletionRequest{Model: "chat-model", Kind: rt.KindChat, Origin: "mesh",
		Messages: []rt.Message{{Role: "user", Content: "PRIVATE PROMPT TEXT"}}, Params: rt.GenerationParams{Seed: 1, MaxTokens: 500}})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := ts.Recv(); err != nil {
			t.Fatal(err)
		}
	}
	// And two decisions through the local API.
	for range 2 {
		if status, _ := postSystemOne(t, srv, `{"model":"flock/laya","state":"PRIVATE STATE","questions":{"a":{"type":"noul","instructions":"q"}}}`); status != http.StatusOK {
			t.Fatalf("systemone = %d", status)
		}
	}

	ra, raw = getRequests(t, srv, "")
	if len(ra.Inflight) != 1 {
		t.Fatalf("inflight = %+v", ra.Inflight)
	}
	in := ra.Inflight[0]
	if in.Model != "chat-model" || in.Kind != gen.RequestKindChat || in.Origin != gen.Mesh || in.Tokens != 3 ||
		in.ElapsedMs < 0 || in.StartedAt.IsZero() || !strings.HasPrefix(in.Id, "r") {
		t.Fatalf("in-flight row = %+v", in)
	}
	chat := ra.Models[0]
	if chat.Inflight != 1 || chat.InflightByKind["chat"] != 1 || chat.IdleSeconds != nil || chat.LastRequestAt == nil {
		t.Fatalf("busy model = %+v", chat)
	}
	if laya := ra.Models[1]; laya.Inflight != 0 || laya.LastRequestAt == nil || laya.IdleSeconds == nil {
		t.Fatalf("decision model after requests = %+v", laya)
	}
	if len(ra.Recent) != 2 {
		t.Fatalf("recent = %+v", ra.Recent)
	}
	for _, r := range ra.Recent {
		if r.Model != "laya-q8_0" || r.Kind != gen.RequestKindDecision || r.Origin != gen.Local ||
			r.Outcome != gen.RequestOutcomeOk || r.CompletionTokens != 0 || r.PromptTokens == 0 || r.DurationMs < 0 {
			t.Fatalf("recent row = %+v", r)
		}
	}
	if strings.Contains(raw, "PRIVATE") {
		t.Fatalf("request content in the activity view: %s", raw)
	}

	// The same summary rides on Status.
	resp := apiGet(t, srv, "/api/v1/status")
	var st gen.Status
	_ = json.NewDecoder(resp.Body).Decode(&st)
	_ = resp.Body.Close()
	if len(st.Activity.Models) != 2 || st.Activity.Models[0].Inflight != 1 || st.Activity.Operations == nil {
		t.Fatalf("status.activity = %+v", st.Activity)
	}

	// Cancel the chat: it moves to recent as cancelled; ?recent bounds the list.
	_ = ts.Close()
	ra, _ = getRequests(t, srv, "?recent=1")
	if len(ra.Inflight) != 0 || len(ra.Recent) != 1 || ra.Recent[0].Outcome != gen.RequestOutcomeCancelled ||
		ra.Recent[0].Id != in.Id || ra.Recent[0].CompletionTokens != 3 {
		t.Fatalf("after cancel = %+v / %+v", ra.Inflight, ra.Recent)
	}
	if ra, _ = getRequests(t, srv, "?recent=0"); len(ra.Recent) != 0 {
		t.Fatalf("recent=0 returned %d rows", len(ra.Recent))
	}

	// Authenticated like the rest of the management API.
	r2, err := http.Get(srv.URL + "/api/v1/requests")
	if err != nil {
		t.Fatal(err)
	}
	_ = r2.Body.Close()
	if r2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated = %d", r2.StatusCode)
	}
}

// request_started and request_finished arrive on the SSE stream with the
// same objects the API returns.
func TestRequestEventsOverSSE(t *testing.T) {
	srv, _ := activityServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	next := func() (string, string) {
		name := ""
		for sc.Scan() {
			line := sc.Text()
			if v, ok := strings.CutPrefix(line, "event: "); ok {
				name = v
			}
			if v, ok := strings.CutPrefix(line, "data: "); ok {
				return name, v
			}
		}
		t.Fatal("event stream ended")
		return "", ""
	}
	if name, data := next(); name != "status" || !strings.Contains(data, `"activity":{"models":[`) {
		t.Fatalf("first event = %s %s", name, data)
	}
	// The subscription is live once the first status arrived.
	if status, _ := postSystemOne(t, srv, `{"model":"laya-q8_0","state":"PRIVATE","questions":{"a":{"type":"noul","instructions":"q"}}}`); status != http.StatusOK {
		t.Fatalf("systemone = %d", status)
	}
	var started gen.InflightRequest
	var finished gen.FinishedRequest
	for started.Id == "" || finished.Id == "" {
		name, data := next()
		if strings.Contains(data, "PRIVATE") {
			t.Fatalf("content in event %s: %s", name, data)
		}
		switch name {
		case "request_started":
			if err := json.Unmarshal([]byte(data), &started); err != nil {
				t.Fatal(err)
			}
		case "request_finished":
			if err := json.Unmarshal([]byte(data), &finished); err != nil {
				t.Fatal(err)
			}
		}
	}
	if started.Model != "laya-q8_0" || started.Kind != gen.RequestKindDecision || started.Origin != gen.Local || started.Tokens != 0 {
		t.Fatalf("request_started = %+v", started)
	}
	if finished.Id != started.Id || finished.Outcome != gen.RequestOutcomeOk || finished.PromptTokens == 0 {
		t.Fatalf("request_finished = %+v (started %+v)", finished, started)
	}
}

// blockingLoader holds a runtime start open until released, the way a
// multi-GB load does.
type blockingLoader struct {
	*rt.MockRuntime
	entered, release chan struct{}
}

func (b blockingLoader) Load(ctx context.Context, m rt.ModelSpec, res rt.ResourceBudget) (rt.Instance, error) {
	close(b.entered)
	<-b.release
	return b.MockRuntime.Load(ctx, m, res)
}

// A runtime starting uses the machine without a request: it shows as a
// `loading` operation (status.activity and /requests) and as
// models_changed loading -> loaded events.
func TestLoadingIsVisible(t *testing.T) {
	srv, _, deps := newOpsServerDeps(t)
	hub := events.NewHub()
	deps.ModelOps.Events = hub
	ch, cancel := hub.Subscribe()
	defer cancel()
	bl := blockingLoader{MockRuntime: rt.NewMockRuntime(0), entered: make(chan struct{}), release: make(chan struct{})}
	deps.ModelOps.Loader = bl

	done := make(chan error, 1)
	go func() { done <- deps.ModelOps.Load(context.Background(), "cat-model") }()
	select {
	case <-bl.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("load never reached the runtime")
	}
	ra, _ := getRequests(t, srv, "")
	if len(ra.Operations) != 1 || ra.Operations[0].Model != "cat-model" || ra.Operations[0].Op != gen.Loading || ra.Operations[0].StartedAt == nil {
		t.Fatalf("operations while loading = %+v", ra.Operations)
	}
	resp := apiGet(t, srv, "/api/v1/status")
	var st gen.Status
	_ = json.NewDecoder(resp.Body).Decode(&st)
	_ = resp.Body.Close()
	if len(st.Activity.Operations) != 1 || st.Activity.Operations[0].Op != gen.Loading {
		t.Fatalf("status.activity while loading = %+v", st.Activity)
	}
	close(bl.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if ra, _ := getRequests(t, srv, ""); len(ra.Operations) != 0 {
		t.Fatalf("operations after load = %+v", ra.Operations)
	}
	var changes []string
	deadline := time.After(5 * time.Second)
	for len(changes) < 2 {
		select {
		case ev := <-ch:
			if m, ok := ev.Data.(map[string]string); ok && ev.Type == "models_changed" && m["model"] == "cat-model" {
				changes = append(changes, m["change"])
			}
		case <-deadline:
			t.Fatalf("events = %v", changes)
		}
	}
	if changes[0] != "loading" || changes[len(changes)-1] != "loaded" {
		t.Fatalf("events = %v, want loading ... loaded", changes)
	}
}
