package localapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teraflock/flockd/internal/engine"
	"github.com/teraflock/flockd/internal/localapi/gen"
	"github.com/teraflock/flockd/internal/modelops"
	"github.com/teraflock/flockd/internal/models"
	rt "github.com/teraflock/flockd/internal/runtime"
)

func TestModelListsReportPartsOfAShardedModel(t *testing.T) {
	files := map[string][]byte{
		"sh-00001-of-00002.gguf": []byte("first shard"),
		"sh-00002-of-00002.gguf": []byte("second shard"),
		"mmproj-F16.gguf":        []byte("proj"),
	}
	art := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(files[strings.TrimPrefix(r.URL.Path, "/")])
	}))
	t.Cleanup(art.Close)
	sha := func(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
	part := func(name string) map[string]any {
		return map[string]any{"url": art.URL + "/" + name, "sha256": sha(files[name]), "size_bytes": len(files[name])}
	}
	h1, h2 := sha(files["sh-00001-of-00002.gguf"]), sha(files["sh-00002-of-00002.gguf"])
	catalog, _ := json.Marshal(map[string]any{"models": []map[string]any{
		{"id": "sh", "sha256": models.CompositeSHA256([]string{h1, h2}), "artifact_url": "", "size_bytes": 23,
			"parts": []map[string]any{part("sh-00001-of-00002.gguf"), part("sh-00002-of-00002.gguf")}, "mmproj": part("mmproj-F16.gguf")},
		{"id": "plain", "sha256": strings.Repeat("a", 64), "artifact_url": art.URL + "/plain", "size_bytes": 1},
	}})
	dir := t.TempDir()
	catPath := filepath.Join(dir, "catalog.json")
	if err := os.WriteFile(catPath, catalog, 0o600); err != nil {
		t.Fatal(err)
	}
	mgr, err := models.NewManager(filepath.Join(dir, "models"), 0, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	eng := engine.New(nil, nil, nil)
	ops := &modelops.Service{Mgr: mgr, Eng: eng, Loader: rt.NewMockRuntime(0), Budget: rt.ResourceBudget{MaxConcurrent: 1}, Log: quietLog(), ManifestPath: catPath}
	srv := httptest.NewServer(New(Deps{Engine: eng, Models: mgr, ModelOps: ops, DataDir: dir, Log: quietLog(), NodeID: "n", Version: "test", Token: testToken}).Handler())
	t.Cleanup(srv.Close)

	resp := apiGet(t, srv, "/api/v1/catalog")
	var cat gen.CatalogList
	if err := json.NewDecoder(resp.Body).Decode(&cat); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	byID := map[string]gen.CatalogEntry{}
	for _, m := range cat.Models {
		byID[m.Id] = m
	}
	if e := byID["sh"]; e.PartsTotal == nil || *e.PartsTotal != 3 || e.PartsDone != nil || e.TotalBytes != nil {
		t.Fatalf("catalog sharded row = %+v", e)
	}
	if e := byID["plain"]; e.PartsTotal != nil {
		t.Fatalf("plain row grew parts: %+v", e)
	}

	resp = apiPost(t, srv, "/api/v1/models/sh/download", "")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("download = %d", resp.StatusCode)
	}
	deadline := time.After(5 * time.Second)
	for {
		resp = apiGet(t, srv, "/api/v1/models")
		var list gen.ModelList
		if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		var row *gen.ModelRow
		for i := range list.Models {
			if list.Models[i].Id == "sh" {
				row = &list.Models[i]
			}
		}
		if row != nil && row.State == "ready" {
			if row.PartsTotal == nil || *row.PartsTotal != 3 || row.PartsDone == nil || *row.PartsDone != 3 {
				t.Fatalf("ready row parts = %+v", row)
			}
			if row.Path == nil || !strings.HasSuffix(*row.Path, filepath.Join("sh", "sh-00001-of-00002.gguf")) {
				t.Fatalf("ready row path = %v", row.Path)
			}
			if row.SizeBytes != 27 {
				t.Fatalf("size_bytes = %d, want every file on disk (27)", row.SizeBytes)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatalf("never ready: %+v", list.Models)
		case <-time.After(10 * time.Millisecond):
		}
	}
}
