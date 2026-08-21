package models

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

const yamlCatalog = `
models:
  - id: llama-3.1-8b-instruct-q4_k_m
    family: llama-3.1
    params_b: 8
    quant: Q4_K_M
    sha256: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    min_vram_mb: 6144
    min_ram_mb: 8192
    license: llama3.1
    artifact_url: https://models.teraflock.dev/llama-3.1-8b-q4.gguf
    size_bytes: 4920000000
    payout_class: small
    context_length: 8192
  - id: nomic-embed-text-v1.5-q8
    family: nomic-embed
    params_b: 0.14
    quant: Q8_0
    sha256: bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
    embeddings: true
`

func TestParseCatalogYAML(t *testing.T) {
	c, err := ParseCatalog([]byte(yamlCatalog))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Models) != 2 {
		t.Fatalf("models = %d", len(c.Models))
	}
	m, ok := c.Find("llama-3.1-8b-instruct-q4_k_m")
	if !ok || m.PayoutClass != "small" || m.ContextLength != 8192 {
		t.Errorf("entry = %+v", m)
	}
	sp := m.Spec()
	if sp.GetId() != m.ID || sp.GetMinVramMb() != 6144 {
		t.Errorf("spec = %+v", sp)
	}
	if e, _ := c.Find("nomic-embed-text-v1.5-q8"); !e.Embeddings {
		t.Error("embeddings flag lost")
	}
}

func TestParseCatalogJSON(t *testing.T) {
	j := `{"models":[{"id":"x","sha256":"cc"}]}`
	c, err := ParseCatalog([]byte(j))
	if err != nil || len(c.Models) != 1 {
		t.Fatalf("json parse: %v", err)
	}
}

func TestParseCatalogRejectsMissingFields(t *testing.T) {
	if _, err := ParseCatalog([]byte(`{"models":[{"id":"x"}]}`)); err == nil {
		t.Fatal("expected error for missing sha256")
	}
}

func TestLoadCatalogFromPathAndURL(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "catalog.yaml")
	if err := os.WriteFile(p, []byte(yamlCatalog), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCatalog(context.Background(), p, "", nil)
	if err != nil || len(c.Models) != 2 {
		t.Fatalf("path load: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(yamlCatalog))
	}))
	defer srv.Close()
	c, err = LoadCatalog(context.Background(), "", srv.URL, nil)
	if err != nil || len(c.Models) != 2 {
		t.Fatalf("url load: %v", err)
	}

	if _, err := LoadCatalog(context.Background(), "", "", nil); err == nil {
		t.Fatal("expected error with no source")
	}
}
