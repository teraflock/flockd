package localapi

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teraflock/flockd/internal/config"
	"github.com/teraflock/flockd/internal/engine"
	"github.com/teraflock/flockd/internal/events"
	"github.com/teraflock/flockd/internal/logging"
	"github.com/teraflock/flockd/internal/modelops"
	"github.com/teraflock/flockd/internal/models"
	rt "github.com/teraflock/flockd/internal/runtime"
)

// newOpsServer builds a server whose ModelOps is backed by a one-model
// catalog, a real manager, and the mock runtime as loader.
func newOpsServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	blob := []byte("tiny gguf artifact")
	sum := sha256.Sum256(blob)

	art := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(blob)
	}))
	t.Cleanup(art.Close)

	dir := t.TempDir()
	catalog := fmt.Sprintf(
		"models:\n  - id: cat-model\n    sha256: %s\n    artifact_url: %s/cat-model\n    size_bytes: %d\n    min_ram_mb: 4096\n",
		hex.EncodeToString(sum[:]), art.URL, len(blob),
	)
	catPath := filepath.Join(dir, "catalog.yaml")
	if err := os.WriteFile(catPath, []byte(catalog), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr, err := models.NewManager(filepath.Join(dir, "models"), 0, quietLog())
	if err != nil {
		t.Fatal(err)
	}

	eng := engine.New(nil, nil, nil)
	mock := rt.NewMockRuntime(0)
	inst, err := mock.Load(context.Background(), rt.ModelSpec{ID: "mock-8b-instruct"}, rt.ResourceBudget{MaxConcurrent: 4})
	if err != nil {
		t.Fatal(err)
	}
	eng.Register(rt.ModelSpec{ID: "mock-8b-instruct"}, inst)

	ops := &modelops.Service{
		Mgr: mgr, Eng: eng, Loader: mock,
		Budget: rt.ResourceBudget{MaxConcurrent: 4},
		Log:    quietLog(), ManifestPath: catPath,
	}
	s := New(Deps{
		Engine:   eng,
		Models:   mgr,
		ModelOps: ops,
		DataDir:  dir,
		Log:      quietLog(),
		NodeID:   "node-test",
		Version:  "test",
		Token:    testToken,
	})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv, dir
}

func apiPost(t *testing.T, srv *httptest.Server, path string, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestHealthIsUnauthenticated(t *testing.T) {
	srv := newTestServer(t, servingGovernor(t))
	resp, err := http.Get(srv.URL + "/api/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", resp.StatusCode)
	}
	var body struct {
		OK      bool   `json:"ok"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.OK || body.Version != "test" {
		t.Fatalf("health body = %+v", body)
	}
}

func TestModelOpsRoutes501WithoutService(t *testing.T) {
	srv := newTestServer(t, servingGovernor(t)) // no ModelOps wired
	resp := apiGet(t, srv, "/api/v1/catalog")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("catalog without service = %d, want 501", resp.StatusCode)
	}
}

func TestCatalogDownloadLoadFlow(t *testing.T) {
	srv, _ := newOpsServer(t)

	// Catalog lists the model, not installed.
	resp := apiGet(t, srv, "/api/v1/catalog")
	var cat struct {
		Models []struct {
			ID        string `json:"id"`
			Installed bool   `json:"installed"`
			MinRAMMB  uint64 `json:"min_ram_mb"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cat); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(cat.Models) != 1 || cat.Models[0].ID != "cat-model" || cat.Models[0].Installed || cat.Models[0].MinRAMMB != 4096 {
		t.Fatalf("catalog = %+v", cat)
	}

	// Trigger the download and wait for ready.
	resp = apiPost(t, srv, "/api/v1/models/cat-model/download", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("download = %d, want 202", resp.StatusCode)
	}
	deadline := time.After(5 * time.Second)
	for {
		resp = apiGet(t, srv, "/api/v1/models")
		var list struct {
			Models []ModelRow `json:"models"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		var ready bool
		for _, m := range list.Models {
			if m.ID == "cat-model" && m.State == "ready" {
				ready = true
			}
		}
		if ready {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("model never ready: %+v", list.Models)
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Re-download reports ready without starting anything.
	resp = apiPost(t, srv, "/api/v1/models/cat-model/download", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("re-download = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// Load, make default, unload.
	resp = apiPost(t, srv, "/api/v1/models/cat-model/load", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("load = %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = apiPost(t, srv, "/api/v1/models/cat-model/default", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("default = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = apiGet(t, srv, "/api/v1/status")
	var st struct {
		DefaultModel string `json:"default_model"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if st.DefaultModel != "cat-model" {
		t.Fatalf("default_model = %q", st.DefaultModel)
	}

	resp = apiPost(t, srv, "/api/v1/models/cat-model/unload", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unload = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Unknown id 404s.
	resp = apiPost(t, srv, "/api/v1/models/nope/download", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown download = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestLimitsPersistToOverlay(t *testing.T) {
	srv, dir := newOpsServer(t)
	// This server has no governor: PUT should 501 and write nothing…
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/v1/limits",
		strings.NewReader(`{"serve_policy":"always","serve_on_battery":true,"max_temp_celsius":88}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("put limits without governor = %d", resp.StatusCode)
	}

	// …while a governor-backed server persists the overlay.
	gsrv := newTestServerWithDataDir(t, dir)
	req, _ = http.NewRequest(http.MethodPut, gsrv.URL+"/api/v1/limits",
		strings.NewReader(`{"serve_policy":"always","serve_on_battery":true,"max_temp_celsius":88,"schedule":["22:00-08:00"]}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put limits = %d", resp.StatusCode)
	}
	raw, err := os.ReadFile(config.LimitsPath(dir))
	if err != nil {
		t.Fatalf("limits overlay not written: %v", err)
	}
	for _, want := range []string{`serve_policy = "always"`, "serve_on_battery = true", "max_temp_celsius = 88", `"22:00-08:00"`} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Fatalf("overlay missing %q:\n%s", want, raw)
		}
	}
}

func newTestServerWithDataDir(t *testing.T, dir string) *httptest.Server {
	t.Helper()
	s := New(Deps{
		Engine:   engine.New(nil, nil, nil),
		Governor: servingGovernor(t),
		DataDir:  dir,
		Log:      quietLog(),
		NodeID:   "node-test",
		Version:  "test",
		Token:    testToken,
	})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func TestEventsStreamRicherEvents(t *testing.T) {
	hub := events.NewHub()
	log, ring := logging.New("info", "text")
	eng := engine.New(nil, nil, nil)
	s := New(Deps{
		Engine:  eng,
		Events:  hub,
		LogRing: ring,
		Log:     quietLog(),
		NodeID:  "node-test",
		Version: "test",
		Token:   testToken,
	})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/events?logs=1&token="+testToken, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })

	events := make(chan [2]string, 16) // [event, data]
	go func() {
		sc := bufio.NewScanner(resp.Body)
		var ev string
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "event: ") {
				ev = strings.TrimPrefix(line, "event: ")
			}
			if strings.HasPrefix(line, "data: ") {
				events <- [2]string{ev, strings.TrimPrefix(line, "data: ")}
			}
		}
	}()

	next := func(wantType string) string {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case e := <-events:
				if e[0] == wantType {
					return e[1]
				}
				// Interleaved status ticks are expected; skip them.
			case <-deadline:
				t.Fatalf("no %q event", wantType)
			}
		}
	}

	next("status") // initial snapshot

	hub.Publish("model_progress", map[string]any{"model": "m1", "received_bytes": 10, "total_bytes": 100})
	data := next("model_progress")
	if !strings.Contains(data, `"model":"m1"`) {
		t.Fatalf("model_progress data = %s", data)
	}

	hub.Publish("models_changed", map[string]string{"model": "m1", "change": "loaded"})
	next("models_changed")

	log.Info("streamed line", "req", "x")
	data = next("log")
	if !strings.Contains(data, "streamed line") {
		t.Fatalf("log data = %s", data)
	}
}
