package localapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teraflock/flockd/internal/engine"
	"github.com/teraflock/flockd/internal/governor"
	"github.com/teraflock/flockd/internal/localapi/gen"
	"github.com/teraflock/flockd/internal/modelops"
	"github.com/teraflock/flockd/internal/models"
	rt "github.com/teraflock/flockd/internal/runtime"
)

// rejectingInstance is a decision model whose runtime rejects every
// input, the way llama-server answers 400.
type rejectingInstance struct{ rt.Instance }

func (rejectingInstance) Complete(context.Context, rt.CompletionRequest) (rt.TokenStream, error) {
	return nil, &rt.InvalidInputError{Msg: "input (3028 tokens) is larger than the max context size (2048 tokens)"}
}

// newSystemOneServer serves a chat model (the default), a decision model
// and a decision model whose runtime rejects everything.
func newSystemOneServer(t *testing.T, gov *governor.Governor) *httptest.Server {
	t.Helper()
	var admit engine.Admitter
	if gov != nil {
		admit = gov
	}
	eng := engine.New(admit, nil, nil)
	mock := rt.NewMockRuntime(0)
	for _, spec := range []rt.ModelSpec{{ID: "mock-8b-instruct"}, {ID: "laya", Decision: true}} {
		inst, err := mock.Load(context.Background(), spec, rt.ResourceBudget{MaxConcurrent: 4})
		if err != nil {
			t.Fatal(err)
		}
		eng.Register(spec, inst)
	}
	eng.Register(rt.ModelSpec{ID: "picky", Decision: true}, rejectingInstance{})
	s := New(Deps{Engine: eng, Governor: gov, Log: quietLog(), NodeID: "node-test", Version: "test", Standalone: true, Token: testToken})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func postSystemOne(t *testing.T, srv *httptest.Server, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/v1/systemone", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(raw)
}

const systemOneBody = `{
  "model": "%s",
  "state": "Help! My payouts have been failing for 3 days.",
  "questions": {
    "zeta_department": {"type": "choice", "instructions": "Which team should handle this?",
        "criteria": {"technical": null, "billing": "Payments, invoicing, refunds", "account": {"covers": ["login"]}}},
    "urgency": {"type": "score", "instructions": "How urgent is this?",
        "criteria": ["can wait", "this week", {"label": "today"}, "right now"]},
    "escalate": {"type": "noul", "instructions": "Does this need a human within the hour?"},
    "angry": {"type": "noul", "instructions": "Is the customer angry?", "criteria": {"true": "upset", "false": "calm"}}
  }
}`

// inOrder asserts the needles appear in s one after another, in that
// order: document order in the raw response.
func inOrder(t *testing.T, s string, needles ...string) {
	t.Helper()
	pos := 0
	for _, n := range needles {
		i := strings.Index(s[pos:], n)
		if i < 0 {
			t.Fatalf("%s is missing or out of order in:\n%s", n, s)
		}
		pos += i + len(n)
	}
}

func TestSystemOne(t *testing.T) {
	srv := newSystemOneServer(t, servingGovernor(t))
	status, raw := postSystemOne(t, srv, fmt.Sprintf(systemOneBody, "flock/laya"))
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, raw)
	}

	// Order in the bytes on the wire: answers in request order, options
	// and levels in the order written (none of them alphabetical).
	inOrder(t, raw, `"model":"laya"`, `"answers":{`, `"zeta_department":{"type":"choice"`, `"urgency":{"type":"score"`,
		`"escalate":{"type":"noul"`, `"angry":{"type":"noul"`, `"usage":{"input_tokens":`)
	inOrder(t, raw, `"probabilities":{"technical":`, `"billing":`, `"account":`, `"confidence":`)
	// legend is rebuilt from the request's criteria, leaves verbatim.
	inOrder(t, raw, `"legend":{"0":"can wait","1":"this week","2":{"label":"today"},"3":"right now"}`,
		`"probabilities":{"0":`, `"1":`, `"2":`, `"3":`)
	if !strings.HasSuffix(raw, `"output_tokens":0}}`) {
		t.Fatalf("usage: %s", raw)
	}

	// And it is the documented shape.
	var resp struct {
		Model   string `json:"model"`
		Answers map[string]struct {
			Type          string             `json:"type"`
			Choice        *string            `json:"choice"`
			Score         *float64           `json:"score"`
			Noul          *float64           `json:"noul"`
			Legend        map[string]any     `json:"legend"`
			Probabilities map[string]float64 `json:"probabilities"`
			Confidence    *float64           `json:"confidence"`
		} `json:"answers"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, raw)
	}
	if resp.Model != "laya" || len(resp.Answers) != 4 || resp.Usage.InputTokens == 0 || resp.Usage.OutputTokens != 0 {
		t.Fatalf("response = %+v", resp)
	}
	c := resp.Answers["zeta_department"]
	if c.Type != "choice" || c.Choice == nil || len(c.Probabilities) != 3 || c.Confidence == nil || c.Legend != nil || c.Noul != nil {
		t.Fatalf("choice answer = %+v", c)
	}
	if _, ok := c.Probabilities[*c.Choice]; !ok {
		t.Fatalf("choice %q is not one of the options", *c.Choice)
	}
	s := resp.Answers["urgency"]
	if s.Type != "score" || s.Score == nil || len(s.Legend) != 4 || len(s.Probabilities) != 4 || s.Confidence == nil || s.Choice != nil {
		t.Fatalf("score answer = %+v", s)
	}
	for _, id := range []string{"escalate", "angry"} {
		n := resp.Answers[id]
		if n.Type != "noul" || n.Noul == nil || n.Probabilities != nil || n.Confidence != nil {
			t.Fatalf("noul answer %s = %+v", id, n)
		}
	}
}

func TestSystemOneErrors(t *testing.T) {
	srv := newSystemOneServer(t, servingGovernor(t))
	one := func(model string) string {
		return `{"model":"` + model + `","state":"s","questions":{"a":{"type":"noul","instructions":"q"}}}`
	}
	cases := []struct {
		name, body string
		status     int
		want       string
	}{
		{"chat model", one("mock-8b-instruct"), http.StatusNotFound, "/v1/chat/completions"},
		{"unknown model", one("nope"), http.StatusNotFound, "model_not_found"},
		{"no model", `{"state":"s","questions":{"a":{"type":"noul","instructions":"q"}}}`, http.StatusUnprocessableEntity, "model is required"},
		{"validation names the path", `{"model":"laya","state":"s","questions":{"ok":{"type":"noul","instructions":"q"},"team":{"type":"choice","instructions":"q","criteria":{"only":null}}}}`,
			http.StatusUnprocessableEntity, "questions.team.criteria must have 2 to 255 options, got 1"},
		{"images", `{"model":"laya","state":"s","images":["data:image/png;base64,AAAA"],"questions":{"a":{"type":"noul","instructions":"q"}}}`,
			http.StatusUnprocessableEntity, "image input is not supported by this model"},
		{"runtime rejects the input", one("picky"), http.StatusUnprocessableEntity, "larger than the max context size"},
		{"malformed", `{"model":`, http.StatusBadRequest, "invalid JSON body"},
		{"not an object", `[1,2]`, http.StatusBadRequest, "invalid JSON body"},
	}
	for _, c := range cases {
		status, raw := postSystemOne(t, srv, c.body)
		if status != c.status {
			t.Errorf("%s: status %d, want %d: %s", c.name, status, c.status, raw)
			continue
		}
		var e gen.Error
		if err := json.Unmarshal([]byte(raw), &e); err != nil || e.Error.Message == "" || e.Error.Type == "" {
			t.Errorf("%s: not the error envelope: %s", c.name, raw)
		}
		if !strings.Contains(raw, c.want) {
			t.Errorf("%s: %s does not contain %q", c.name, raw, c.want)
		}
	}

	// Validation runs before the model lookup is ever a 404 for the wrong
	// reason: a 404 carries the code clients switch on.
	_, raw := postSystemOne(t, srv, one("mock-8b-instruct"))
	var e gen.Error
	_ = json.Unmarshal([]byte(raw), &e)
	if e.Error.Code == nil || *e.Error.Code != "model_not_found" {
		t.Fatalf("404 code = %v: %s", e.Error.Code, raw)
	}
}

func TestSystemOneYieldedNodeReturns503(t *testing.T) {
	idle := &governor.FakeIdleSource{}
	gov := governor.New(governor.Policy{Serve: "idle-only", IdleAfter: time.Minute}, idle, &governor.FakePowerSource{}, nil, quietLog())
	srv := newSystemOneServer(t, gov)
	status, raw := postSystemOne(t, srv, `{"model":"laya","state":"s","questions":{"a":{"type":"noul","instructions":"q"}}}`)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status %d: %s", status, raw)
	}
}

// A decision model that is in the catalog and on disk but not loaded is
// loaded on demand; the catalog listing exposes `decision` so UIs can
// tell it from a chat model.
func TestSystemOneLoadsCatalogDecisionModelOnDemand(t *testing.T) {
	blob := []byte("tiny decision gguf")
	sum := sha256.Sum256(blob)
	art := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(blob) }))
	t.Cleanup(art.Close)
	dir := t.TempDir()
	// The flat catalog JSON the models repo publishes: `decision` next to
	// `embeddings` on every entry.
	catalog := fmt.Sprintf(`{"models":[
	  {"id":"laya","family":"laya","sha256":%q,"artifact_url":"%s/laya","size_bytes":%d,"context_length":512,"embeddings":false,"decision":true},
	  {"id":"chat","family":"qwen","sha256":%q,"artifact_url":"%s/chat","size_bytes":%d,"context_length":4096,"embeddings":false,"decision":false}]}`,
		hex.EncodeToString(sum[:]), art.URL, len(blob), hex.EncodeToString(sum[:]), art.URL, len(blob))
	catPath := filepath.Join(dir, "catalog.json")
	if err := os.WriteFile(catPath, []byte(catalog), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr, err := models.NewManager(filepath.Join(dir, "models"), 0, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	eng := engine.New(nil, nil, nil)
	ops := &modelops.Service{Mgr: mgr, Eng: eng, Loader: rt.NewMockRuntime(0), Budget: rt.ResourceBudget{MaxConcurrent: 2}, Log: quietLog(), ManifestPath: catPath}
	s := New(Deps{Engine: eng, Models: mgr, ModelOps: ops, DataDir: dir, Log: quietLog(), NodeID: "node-test", Version: "test", Token: testToken})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	body := `{"model":"laya","state":"s","questions":{"a":{"type":"noul","instructions":"q"}}}`
	// Not on disk: a 404, never a download triggered by a request.
	if status, raw := postSystemOne(t, srv, body); status != http.StatusNotFound {
		t.Fatalf("not installed: status %d: %s", status, raw)
	}
	for _, id := range []string{"laya", "chat"} {
		if err := ops.Fetch(context.Background(), id, models.OriginOperator); err != nil {
			t.Fatal(err)
		}
	}
	status, raw := postSystemOne(t, srv, body)
	if status != http.StatusOK || !strings.Contains(raw, `"a":{"type":"noul","noul":`) {
		t.Fatalf("on-demand load: status %d: %s", status, raw)
	}
	loaded := eng.Models()
	if len(loaded) != 1 || loaded[0].Spec.ID != "laya" || !loaded[0].Spec.Decision {
		t.Fatalf("loaded = %+v, want laya as a decision model", loaded)
	}
	// An installed chat model is still not a decision model.
	if status, raw := postSystemOne(t, srv, strings.Replace(body, `"laya"`, `"chat"`, 1)); status != http.StatusNotFound {
		t.Fatalf("chat model: status %d: %s", status, raw)
	}

	resp := apiGet(t, srv, "/api/v1/catalog")
	defer resp.Body.Close()
	var list gen.CatalogList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, m := range list.Models {
		got[m.Id] = m.Decision
	}
	if len(got) != 2 || !got["laya"] || got["chat"] {
		t.Fatalf("catalog decision flags = %v", got)
	}
}
