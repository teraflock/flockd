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

	"github.com/teraflock/flockd/internal/engine"
	"github.com/teraflock/flockd/internal/localapi/gen"
	"github.com/teraflock/flockd/internal/modelops"
	"github.com/teraflock/flockd/internal/models"
	rt "github.com/teraflock/flockd/internal/runtime"
)

// aliasServer is a node whose catalog has two quants of a decision
// manifest (laya) and two of a chat manifest (qwen3-8b), every artifact
// downloadable, nothing installed.
func aliasServer(t *testing.T) (*httptest.Server, *modelops.Service, *engine.Engine) {
	t.Helper()
	art := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("gguf " + filepath.Base(r.URL.Path)))
	}))
	t.Cleanup(art.Close)
	entry := func(id, quant string, decision bool) string {
		sum := sha256.Sum256([]byte("gguf " + id))
		return fmt.Sprintf(`{"id":%q,"quant":%q,"family":"f","sha256":%q,"artifact_url":"%s/%s","size_bytes":%d,"context_length":512,"embeddings":false,"decision":%v}`,
			id, quant, hex.EncodeToString(sum[:]), art.URL, id, len("gguf "+id), decision)
	}
	dir := t.TempDir()
	catPath := filepath.Join(dir, "catalog.json")
	catalog := `{"models":[` + strings.Join([]string{
		entry("laya-q8_0", "Q8_0", true), entry("laya-bf16", "BF16", true),
		entry("qwen3-8b-q4_k_m", "Q4_K_M", false), entry("qwen3-8b-q8_0", "Q8_0", false),
	}, ",") + `]}`
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
	return srv, ops, eng
}

func postJSON(t *testing.T, url, body string) (int, map[string]any, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return resp.StatusCode, m, string(raw)
}

// `flock/<manifest id>` and the bare manifest id resolve to a quant of
// that manifest on every /v1 route — the quant this node already has,
// else the manifest's first — and the response names the concrete id.
func TestFlockAliasResolvesToTheNodesQuant(t *testing.T) {
	srv, ops, eng := aliasServer(t)
	ctx := context.Background()
	dec := func(model string) string {
		return `{"model":"` + model + `","state":"s","questions":{"a":{"type":"noul","instructions":"q"}}}`
	}
	chat := func(model string) string {
		return `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}],"max_tokens":4}`
	}

	// Nothing installed: the alias names a model this node does not have
	// (the message keeps what the caller wrote).
	status, _, raw := postJSON(t, srv.URL+"/v1/systemone", dec("flock/laya"))
	if status != http.StatusNotFound || !strings.Contains(raw, `flock/laya`) {
		t.Fatalf("nothing installed: %d %s", status, raw)
	}

	// Only the SECOND quant is on disk: the alias picks it over the
	// manifest's first quant, and the model is loaded on demand.
	if err := ops.Fetch(ctx, "laya-bf16", models.OriginOperator); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"flock/laya", "laya", "laya-bf16", "flock/laya-bf16"} {
		status, m, raw := postJSON(t, srv.URL+"/v1/systemone", dec(name))
		if status != http.StatusOK || m["model"] != "laya-bf16" {
			t.Fatalf("%s with bf16 installed: %d %s", name, status, raw)
		}
	}

	// Both quants loaded: catalog order decides (the gateway's choice).
	if err := ops.Load(ctx, "laya-q8_0"); err != nil {
		t.Fatal(err)
	}
	if status, m, raw := postJSON(t, srv.URL+"/v1/systemone", dec("flock/laya")); status != http.StatusOK || m["model"] != "laya-q8_0" {
		t.Fatalf("both loaded: %d %s", status, raw)
	}
	// One loaded, the other only on disk: loaded wins.
	if err := ops.Unload(ctx, "laya-q8_0"); err != nil {
		t.Fatal(err)
	}
	if status, m, raw := postJSON(t, srv.URL+"/v1/systemone", dec("laya")); status != http.StatusOK || m["model"] != "laya-bf16" {
		t.Fatalf("bf16 loaded, q8_0 on disk: %d %s", status, raw)
	}

	// The chat routes resolve the same way.
	if err := ops.Load(ctx, "qwen3-8b-q8_0"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"flock/qwen3-8b", "qwen3-8b", "qwen3-8b-q8_0"} {
		status, m, raw := postJSON(t, srv.URL+"/v1/chat/completions", chat(name))
		if status != http.StatusOK || m["model"] != "qwen3-8b-q8_0" {
			t.Fatalf("chat %s: %d %s", name, status, raw)
		}
		status, m, raw = postJSON(t, srv.URL+"/v1/completions", `{"model":"`+name+`","prompt":"hi","max_tokens":4}`)
		if status != http.StatusOK || m["model"] != "qwen3-8b-q8_0" {
			t.Fatalf("completions %s: %d %s", name, status, raw)
		}
		status, m, raw = postJSON(t, srv.URL+"/v1/embeddings", `{"model":"`+name+`","input":["a"]}`)
		if status != http.StatusOK || m["model"] != "qwen3-8b-q8_0" {
			t.Fatalf("embeddings %s: %d %s", name, status, raw)
		}
	}
	// An alias never crosses kinds: flock/laya on a chat route is the
	// decision-model refusal, flock/qwen3-8b on systemone is a 404.
	if status, _, raw := postJSON(t, srv.URL+"/v1/chat/completions", chat("flock/laya")); status != http.StatusNotFound ||
		!strings.Contains(raw, `model \"laya-bf16\" is a decision model: use POST /v1/systemone`) {
		t.Fatalf("chat on flock/laya: %d %s", status, raw)
	}
	if status, _, raw := postJSON(t, srv.URL+"/v1/systemone", dec("flock/qwen3-8b")); status != http.StatusNotFound {
		t.Fatalf("systemone on a chat alias: %d %s", status, raw)
	}
	// An unknown alias is a 404, and no request names nothing-in-particular.
	if status, _, raw := postJSON(t, srv.URL+"/v1/chat/completions", chat("flock/nope")); status != http.StatusNotFound {
		t.Fatalf("unknown alias: %d %s", status, raw)
	}

	// The default model can only be a chat model.
	if d := eng.DefaultModel(); d != "qwen3-8b-q8_0" {
		t.Fatalf("default = %q, want the chat model (a decision model was loaded first)", d)
	}
	resp := apiPost(t, srv, "/api/v1/models/laya-bf16/default", "")
	defer resp.Body.Close()
	var e gen.Error
	_ = json.NewDecoder(resp.Body).Decode(&e)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(e.Error.Message, "is a decision model") {
		t.Fatalf("set default to a decision model: %d %+v", resp.StatusCode, e)
	}
	if d := eng.DefaultModel(); d != "qwen3-8b-q8_0" {
		t.Fatalf("default changed to %q", d)
	}
}

// Without a catalog (mock runtime) the alias resolves against the loaded
// models' own id and quant.
func TestFlockAliasWithoutCatalog(t *testing.T) {
	eng := engine.New(nil, nil, nil)
	mock := rt.NewMockRuntime(0)
	for _, spec := range []rt.ModelSpec{{ID: "llama-3.2-3b-instruct-q4_k_m", Quant: "Q4_K_M"}, {ID: "laya-q8_0", Quant: "Q8_0", Decision: true}} {
		inst, err := mock.Load(context.Background(), spec, rt.ResourceBudget{})
		if err != nil {
			t.Fatal(err)
		}
		eng.Register(spec, inst)
	}
	s := New(Deps{Engine: eng, Log: quietLog(), NodeID: "n", Version: "test", Token: testToken})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	status, m, raw := postJSON(t, srv.URL+"/v1/systemone", `{"model":"flock/laya","state":"s","questions":{"a":{"type":"noul","instructions":"q"}}}`)
	if status != http.StatusOK || m["model"] != "laya-q8_0" {
		t.Fatalf("systemone: %d %s", status, raw)
	}
	status, m, raw = postJSON(t, srv.URL+"/v1/chat/completions", `{"model":"flock/llama-3.2-3b-instruct","messages":[{"role":"user","content":"hi"}],"max_tokens":4}`)
	if status != http.StatusOK || m["model"] != "llama-3.2-3b-instruct-q4_k_m" {
		t.Fatalf("chat: %d %s", status, raw)
	}
}
