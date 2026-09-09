package modelops

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/teraflock/flockd/internal/engine"
	"github.com/teraflock/flockd/internal/models"
	rt "github.com/teraflock/flockd/internal/runtime"
)

// captureLoader records what the service asks the runtime to load.
type captureLoader struct {
	mock rt.Runtime
	mu   sync.Mutex
	last rt.ModelSpec
}

func (c *captureLoader) Load(ctx context.Context, m rt.ModelSpec, res rt.ResourceBudget) (rt.Instance, error) {
	c.mu.Lock()
	c.last = m
	c.mu.Unlock()
	return c.mock.Load(ctx, m, res)
}

func TestLoadHandsTheRuntimeEveryFileOfAShardedModel(t *testing.T) {
	parts := map[string][]byte{
		"vl-00001-of-00002.gguf": []byte("shard one"),
		"vl-00002-of-00002.gguf": []byte("shard two!"),
		"mmproj-F16.gguf":        []byte("projector"),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		blob, ok := parts[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(blob)
	}))
	t.Cleanup(srv.Close)

	part := func(name string) map[string]any {
		return map[string]any{"url": srv.URL + "/" + name, "sha256": shaOf(parts[name]), "size_bytes": len(parts[name])}
	}
	h1, h2 := shaOf(parts["vl-00001-of-00002.gguf"]), shaOf(parts["vl-00002-of-00002.gguf"])
	catalog, _ := json.Marshal(map[string]any{"models": []map[string]any{{
		"id": "vl", "sha256": models.CompositeSHA256([]string{h1, h2}), "artifact_url": "",
		"size_bytes": len(parts["vl-00001-of-00002.gguf"]) + len(parts["vl-00002-of-00002.gguf"]),
		"parts":      []map[string]any{part("vl-00001-of-00002.gguf"), part("vl-00002-of-00002.gguf")},
		"mmproj":     part("mmproj-F16.gguf"),
	}}})
	dir := t.TempDir()
	catPath := filepath.Join(dir, "catalog.json")
	if err := os.WriteFile(catPath, catalog, 0o600); err != nil {
		t.Fatal(err)
	}
	mgr, err := models.NewManager(filepath.Join(dir, "models"), 0, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	loader := &captureLoader{mock: rt.NewMockRuntime(0)}
	s := &Service{
		Mgr: mgr, Eng: engine.New(nil, nil, nil), Loader: loader,
		Budget: rt.ResourceBudget{MaxConcurrent: 2}, Log: quietLog(), ManifestPath: catPath,
	}
	if err := s.Load(context.Background(), "vl"); err != nil {
		t.Fatal(err)
	}
	loader.mu.Lock()
	got := loader.last
	loader.mu.Unlock()
	base := filepath.Join(dir, "models", "vl")
	if got.Path != filepath.Join(base, "vl-00001-of-00002.gguf") {
		t.Fatalf("Path = %s", got.Path)
	}
	if got.MmprojPath != filepath.Join(base, "mmproj-F16.gguf") {
		t.Fatalf("MmprojPath = %s", got.MmprojPath)
	}
	if want := int64(len(parts["vl-00001-of-00002.gguf"]) + len(parts["vl-00002-of-00002.gguf"]) + len(parts["mmproj-F16.gguf"])); got.SizeBytes != want {
		t.Fatalf("SizeBytes = %d, want %d", got.SizeBytes, want)
	}
	if got.SHA256 != models.CompositeSHA256([]string{h1, h2}) {
		t.Fatalf("SHA256 = %s, want the composite id", got.SHA256)
	}
}
