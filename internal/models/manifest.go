// Package models manages the local model cache: catalog manifests
// (teraflock/models format), resumable GGUF downloads with SHA256
// verification, and LRU eviction under the operator's disk budget.
package models

import (
	"context"
	"encoding/json"
	"errors"
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
	// Decision marks a typed decision model (served for /v1/systemone,
	// never for chat). Mutually exclusive with Embeddings; absent (false)
	// in catalogs that predate the field.
	Decision bool `yaml:"decision" json:"decision"`
	// Parts lists the shards of a sharded GGUF in series order
	// (<name>-00001-of-0000N.gguf ...), each with its own hash. Empty for a
	// single-file model. When set, ArtifactURL is empty, SizeBytes is the
	// total across parts and SHA256 is the composite id (CompositeSHA256),
	// never a file hash.
	Parts []CatalogPart `yaml:"parts" json:"parts,omitempty"`
	// Mmproj is the optional vision projector sidecar (mmproj-*.gguf)
	// llama-server takes via --mmproj; verified and stored like a part,
	// not counted in SizeBytes.
	Mmproj *CatalogPart `yaml:"mmproj" json:"mmproj,omitempty"`
}

// CatalogPart is one pinned file of a multi-file artifact: a GGUF shard or
// the mmproj sidecar. Same three names as the proto ArtifactPart and the
// catalog YAML so nothing is renamed between them.
type CatalogPart struct {
	URL       string `yaml:"url" json:"url"`
	SHA256    string `yaml:"sha256" json:"sha256"`
	SizeBytes uint64 `yaml:"size_bytes" json:"size_bytes"`
}

func (p CatalogPart) proto() *typesv1.ArtifactPart {
	return &typesv1.ArtifactPart{Url: p.URL, Sha256: p.SHA256, SizeBytes: p.SizeBytes}
}

// Spec converts a catalog entry to the proto ModelSpec.
func (m CatalogModel) Spec() *typesv1.ModelSpec {
	spec := &typesv1.ModelSpec{
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
		Decision:      m.Decision,
	}
	for _, p := range m.Parts {
		spec.Parts = append(spec.Parts, p.proto())
	}
	if m.Mmproj != nil {
		spec.Mmproj = m.Mmproj.proto()
	}
	return spec
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
	c, _, err := FetchCatalog(ctx, path, url, client, CatalogStamp{})
	return c, err
}

// CatalogStamp is the HTTP validators of a fetched catalog (ETag and
// Last-Modified). Handing the last stamp back to FetchCatalog makes the
// request conditional, so a periodic refresh of an unchanged catalog costs
// a 304 instead of the whole document.
type CatalogStamp struct {
	ETag         string
	LastModified string
}

// ErrCatalogNotModified is returned by FetchCatalog when the server says
// the catalog still matches the stamp it was given.
var ErrCatalogNotModified = errors.New("models: catalog not modified")

// FetchCatalog is LoadCatalog with conditional requests. prev is the stamp
// of the copy the caller already holds (zero = unconditional); when the
// server answers 304 the error is ErrCatalogNotModified and the caller
// keeps its copy. A local path is always re-read.
func FetchCatalog(ctx context.Context, path, url string, client *http.Client, prev CatalogStamp) (*Catalog, CatalogStamp, error) {
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, CatalogStamp{}, fmt.Errorf("models: read catalog %s: %w", path, err)
		}
		c, err := ParseCatalog(raw)
		return c, CatalogStamp{}, err
	}
	if url == "" {
		return nil, CatalogStamp{}, fmt.Errorf("models: no catalog configured (set models.manifest_path or models.manifest_url)")
	}
	if client == nil {
		client = &http.Client{Timeout: time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, CatalogStamp{}, fmt.Errorf("models: catalog request: %w", err)
	}
	if prev.ETag != "" {
		req.Header.Set("If-None-Match", prev.ETag)
	}
	if prev.LastModified != "" {
		req.Header.Set("If-Modified-Since", prev.LastModified)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, CatalogStamp{}, fmt.Errorf("models: fetch catalog: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified && prev != (CatalogStamp{}) {
		return nil, prev, ErrCatalogNotModified
	}
	if resp.StatusCode != http.StatusOK {
		return nil, CatalogStamp{}, fmt.Errorf("models: fetch catalog: status %s", resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, CatalogStamp{}, fmt.Errorf("models: read catalog body: %w", err)
	}
	c, err := ParseCatalog(raw)
	if err != nil {
		return nil, CatalogStamp{}, err
	}
	return c, CatalogStamp{ETag: resp.Header.Get("ETag"), LastModified: resp.Header.Get("Last-Modified")}, nil
}

// ManifestID is the catalog manifest a flat entry was emitted from: flat
// ids are `<manifest id>-<quant lowercased>` (models repo, tools/validate
// EmitFlat), and the manifest id is what the public `flock/<id>` alias
// names. "" when the id does not have that shape (an operator's own
// model).
func (m CatalogModel) ManifestID() string {
	return ManifestIDOf(m.ID, m.Quant)
}

// ManifestIDOf is ManifestID for an id and quant held separately.
func ManifestIDOf(id, quant string) string {
	if quant == "" {
		return ""
	}
	base, ok := strings.CutSuffix(id, "-"+strings.ToLower(quant))
	if !ok || base == "" {
		return ""
	}
	return base
}

// Quants returns the entries emitted from one manifest, in catalog order:
// the first is the manifest's first quant, which is what the mesh
// gateway's `flock/<manifest id>` alias serves.
func (c *Catalog) Quants(manifestID string) []CatalogModel {
	if manifestID == "" {
		return nil
	}
	var out []CatalogModel
	for _, m := range c.Models {
		if m.ManifestID() == manifestID {
			out = append(out, m)
		}
	}
	return out
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
