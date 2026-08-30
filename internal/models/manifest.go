// Package models manages the local model cache: catalog manifests
// (teraflock/models format), resumable GGUF downloads with SHA256
// verification, and LRU eviction under the operator's disk budget.
package models

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
	"gopkg.in/yaml.v3"
)

// Catalog is the manifest format of the teraflock/models repo
// (catalog/*.yaml). JSON is accepted too.
type Catalog struct {
	Models []CatalogModel `yaml:"models" json:"models"`
}

// CatalogModel is one curated model entry.
type CatalogModel struct {
	ID string `yaml:"id" json:"id"`
	// DisplayName is the human-readable name (incl. quant) UIs show —
	// ids are ambiguous now that model lines carry point versions
	// (qwen3-8b vs qwen3.8-27b). Empty in pre-field catalogs.
	DisplayName   string  `yaml:"display_name" json:"display_name"`
	Family        string  `yaml:"family" json:"family"`
	ParamsB       float64 `yaml:"params_b" json:"params_b"`
	Quant         string  `yaml:"quant" json:"quant"`
	SHA256        string  `yaml:"sha256" json:"sha256"`
	MinVRAMMB     uint64  `yaml:"min_vram_mb" json:"min_vram_mb"`
	MinRAMMB      uint64  `yaml:"min_ram_mb" json:"min_ram_mb"`
	License       string  `yaml:"license" json:"license"`
	ArtifactURL   string  `yaml:"artifact_url" json:"artifact_url"`
	SizeBytes     uint64  `yaml:"size_bytes" json:"size_bytes"`
	PayoutClass   string  `yaml:"payout_class" json:"payout_class"`
	ContextLength uint32  `yaml:"context_length" json:"context_length"`
	Embeddings    bool    `yaml:"embeddings" json:"embeddings"`
}

// Spec converts a catalog entry to the proto ModelSpec.
func (m CatalogModel) Spec() *typesv1.ModelSpec {
	return &typesv1.ModelSpec{
		Id:            m.ID,
		Family:        m.Family,
		ParamsB:       m.ParamsB,
		Quant:         m.Quant,
		Sha256:        m.SHA256,
		MinVramMb:     m.MinVRAMMB,
		MinRamMb:      m.MinRAMMB,
		License:       m.License,
		ArtifactUrl:   m.ArtifactURL,
		SizeBytes:     m.SizeBytes,
		PayoutClass:   m.PayoutClass,
		ContextLength: m.ContextLength,
		Embeddings:    m.Embeddings,
	}
}

// ParseCatalog decodes YAML or JSON manifest bytes.
func ParseCatalog(raw []byte) (*Catalog, error) {
	var c Catalog
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "{") {
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, fmt.Errorf("models: parse json catalog: %w", err)
		}
	} else if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("models: parse yaml catalog: %w", err)
	}
	for _, m := range c.Models {
		if m.ID == "" || m.SHA256 == "" {
			return nil, fmt.Errorf("models: catalog entry missing id or sha256: %+v", m)
		}
	}
	return &c, nil
}

// LoadCatalog reads a catalog from a local path or URL (one must be set;
// path wins).
func LoadCatalog(ctx context.Context, path, url string, client *http.Client) (*Catalog, error) {
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("models: read catalog %s: %w", path, err)
		}
		return ParseCatalog(raw)
	}
	if url == "" {
		return nil, fmt.Errorf("models: no catalog configured (set models.manifest_path or models.manifest_url)")
	}
	if client == nil {
		client = &http.Client{Timeout: time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("models: catalog request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("models: fetch catalog: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models: fetch catalog: status %s", resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("models: read catalog body: %w", err)
	}
	return ParseCatalog(raw)
}

// Find returns the entry with the given id.
func (c *Catalog) Find(id string) (CatalogModel, bool) {
	for _, m := range c.Models {
		if m.ID == id {
			return m, true
		}
	}
	return CatalogModel{}, false
}
