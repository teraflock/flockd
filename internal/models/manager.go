package models

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"

	typesv1 "github.com/hivegrid/proto/gen/go/hive/types/v1"
)

// ErrSHAMismatch means a downloaded artifact failed verification. The
// daemon refuses to serve hash-mismatched files (SPEC §6).
var ErrSHAMismatch = errors.New("models: sha256 mismatch")

// Manager owns <data_dir>/models: downloads, verification, pins and LRU
// eviction under the disk budget.
type Manager struct {
	Dir       string
	MaxDiskMB int64
	Client    *http.Client
	Log       *slog.Logger

	mu    sync.Mutex
	state cacheState
}

// cacheState is persisted to models.json alongside the artifacts.
type cacheState struct {
	Entries map[string]*cacheEntry `json:"entries"` // key: model id
}

type cacheEntry struct {
	ID        string    `json:"id"`
	SHA256    string    `json:"sha256"`
	SizeBytes int64     `json:"size_bytes"`
	Pinned    bool      `json:"pinned"`
	LastUsed  time.Time `json:"last_used"`
	State     string    `json:"state"` // "downloading", "ready"
}

// NewManager loads (or initializes) the cache state.
func NewManager(dir string, maxDiskMB int64, log *slog.Logger) (*Manager, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("models: mkdir %s: %w", dir, err)
	}
	if log == nil {
		log = slog.Default()
	}
	m := &Manager{
		Dir:       dir,
		MaxDiskMB: maxDiskMB,
		Client:    &http.Client{}, // long downloads: no client timeout, ctx governs
		Log:       log,
		state:     cacheState{Entries: map[string]*cacheEntry{}},
	}
	if raw, err := os.ReadFile(m.statePath()); err == nil {
		if err := json.Unmarshal(raw, &m.state); err != nil {
			log.Warn("models: corrupt cache state, resetting", "err", err)
			m.state = cacheState{Entries: map[string]*cacheEntry{}}
		}
	}
	if m.state.Entries == nil {
		m.state.Entries = map[string]*cacheEntry{}
	}
	return m, nil
}

func (m *Manager) statePath() string { return filepath.Join(m.Dir, "models.json") }

// Path returns where a model artifact lives locally.
func (m *Manager) Path(id string) string { return filepath.Join(m.Dir, id+".gguf") }

func (m *Manager) saveLocked() {
	raw, err := json.MarshalIndent(&m.state, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(m.statePath(), raw, 0o644) //nolint:gosec // cache metadata, not secret
}

// Ensure makes the model available locally: verified cache hit, resumed
// partial download, or full download. Returns the local path.
func (m *Manager) Ensure(ctx context.Context, spec *typesv1.ModelSpec) (string, error) {
	dest := m.Path(spec.GetId())

	m.mu.Lock()
	if e, ok := m.state.Entries[spec.GetId()]; ok && e.State == "ready" {
		e.LastUsed = time.Now()
		m.saveLocked()
		m.mu.Unlock()
		// Trust-but-verify on startup use: cheap stat; full hash was done at
		// download time. Serving re-verification happens in Verify().
		if _, err := os.Stat(dest); err == nil {
			return dest, nil
		}
		// File vanished under us: fall through to re-download.
		m.mu.Lock()
		delete(m.state.Entries, spec.GetId())
		m.saveLocked()
	}
	m.mu.Unlock()

	if err := m.evictForLocked(ctx, int64(spec.GetSizeBytes())); err != nil {
		return "", err
	}

	if err := m.download(ctx, spec, dest); err != nil {
		return "", err
	}

	fi, err := os.Stat(dest)
	if err != nil {
		return "", fmt.Errorf("models: stat %s: %w", dest, err)
	}
	m.mu.Lock()
	m.state.Entries[spec.GetId()] = &cacheEntry{
		ID:        spec.GetId(),
		SHA256:    spec.GetSha256(),
		SizeBytes: fi.Size(),
		LastUsed:  time.Now(),
		State:     "ready",
	}
	m.saveLocked()
	m.mu.Unlock()
	return dest, nil
}

// download performs a resumable fetch into dest via a .partial temp file,
// verifying SHA256 over the complete content before renaming into place.
func (m *Manager) download(ctx context.Context, spec *typesv1.ModelSpec, dest string) error {
	tmp := dest + ".partial"
	var offset int64
	if fi, err := os.Stat(tmp); err == nil {
		offset = fi.Size()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, spec.GetArtifactUrl(), nil)
	if err != nil {
		return fmt.Errorf("models: request %s: %w", spec.GetArtifactUrl(), err)
	}
	if offset > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
	}
	resp, err := m.Client.Do(req)
	if err != nil {
		return fmt.Errorf("models: download %s: %w", spec.GetId(), err)
	}
	defer resp.Body.Close()

	flags := os.O_CREATE | os.O_WRONLY
	switch resp.StatusCode {
	case http.StatusPartialContent:
		flags |= os.O_APPEND
		m.Log.Info("resuming model download", "model", spec.GetId(), "offset", offset)
	case http.StatusOK:
		flags |= os.O_TRUNC // server ignored Range: start over
	default:
		return fmt.Errorf("models: download %s: status %s", spec.GetId(), resp.Status)
	}

	out, err := os.OpenFile(tmp, flags, 0o644) //nolint:gosec
	if err != nil {
		return fmt.Errorf("models: open %s: %w", tmp, err)
	}
	_, cpErr := io.Copy(out, resp.Body)
	closeErr := out.Close()
	if cpErr != nil {
		return fmt.Errorf("models: write %s: %w", spec.GetId(), cpErr) // .partial kept for resume
	}
	if closeErr != nil {
		return fmt.Errorf("models: close %s: %w", tmp, closeErr)
	}

	if err := verifySHA(tmp, spec.GetSha256()); err != nil {
		_ = os.Remove(tmp) // poisoned: do not resume garbage
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return fmt.Errorf("models: finalize %s: %w", dest, err)
	}
	m.Log.Info("model downloaded and verified", "model", spec.GetId(), "sha256", spec.GetSha256())
	return nil
}

// Verify re-hashes a cached artifact against its recorded SHA. Serving
// paths call this before loading a model into the runtime.
func (m *Manager) Verify(id string) error {
	m.mu.Lock()
	e, ok := m.state.Entries[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("models: %s not in cache", id)
	}
	return verifySHA(m.Path(id), e.SHA256)
}

func verifySHA(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("models: open for verify: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("models: hash: %w", err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return fmt.Errorf("%w: %s: got %s want %s", ErrSHAMismatch, filepath.Base(path), got, want)
	}
	return nil
}

// evictForLocked frees space so a new artifact of size need fits inside
// MaxDiskMB, evicting least-recently-used unpinned models first.
func (m *Manager) evictForLocked(_ context.Context, need int64) error {
	if m.MaxDiskMB <= 0 {
		return nil // unlimited
	}
	budget := m.MaxDiskMB * 1024 * 1024
	m.mu.Lock()
	defer m.mu.Unlock()

	total := need
	var candidates []*cacheEntry
	for _, e := range m.state.Entries {
		total += e.SizeBytes
		if !e.Pinned && e.State == "ready" {
			candidates = append(candidates, e)
		}
	}
	if total <= budget {
		return nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].LastUsed.Before(candidates[j].LastUsed)
	})
	for _, e := range candidates {
		if total <= budget {
			break
		}
		m.Log.Info("evicting model (LRU, disk budget)", "model", e.ID, "size_mb", e.SizeBytes/1024/1024)
		if err := os.Remove(m.Path(e.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("models: evict %s: %w", e.ID, err)
		}
		total -= e.SizeBytes
		delete(m.state.Entries, e.ID)
	}
	m.saveLocked()
	if total > budget {
		return fmt.Errorf("models: cannot fit %d MB within disk budget %d MB (pinned models occupy the rest)", need/1024/1024, m.MaxDiskMB)
	}
	return nil
}

// Pin marks a model exempt from eviction; unpin re-allows it.
func (m *Manager) Pin(id string, pinned bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.state.Entries[id]
	if !ok {
		return fmt.Errorf("models: %s not in cache", id)
	}
	e.Pinned = pinned
	m.saveLocked()
	return nil
}

// Remove deletes a model from the cache.
func (m *Manager) Remove(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.state.Entries[id]; !ok {
		return fmt.Errorf("models: %s not in cache", id)
	}
	if err := os.Remove(m.Path(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("models: remove %s: %w", id, err)
	}
	delete(m.state.Entries, id)
	m.saveLocked()
	return nil
}

// Touch refreshes LRU recency (called when a model serves a request).
func (m *Manager) Touch(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.state.Entries[id]; ok {
		e.LastUsed = time.Now()
		m.saveLocked()
	}
}

// Info is a local cache listing row.
type Info struct {
	ID        string    `json:"id"`
	SizeBytes int64     `json:"size_bytes"`
	Pinned    bool      `json:"pinned"`
	LastUsed  time.Time `json:"last_used"`
	State     string    `json:"state"`
}

// List returns cache contents sorted by id.
func (m *Manager) List() []Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Info, 0, len(m.state.Entries))
	for _, e := range m.state.Entries {
		out = append(out, Info{ID: e.ID, SizeBytes: e.SizeBytes, Pinned: e.Pinned, LastUsed: e.LastUsed, State: e.State})
	}
	slices.SortFunc(out, func(a, b Info) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	return out
}
