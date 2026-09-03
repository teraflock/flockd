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
	"strings"
	"sync"
	"time"

	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"

	"github.com/teraflock/flockd/internal/activity"
	"github.com/teraflock/flockd/internal/hardware"
)

// ErrSHAMismatch means a downloaded artifact failed verification. The
// daemon refuses to serve hash-mismatched files (SPEC §6).
var ErrSHAMismatch = errors.New("models: sha256 mismatch")

// ErrInvalidID rejects a model id that is unsafe to use as a path component.
var ErrInvalidID = errors.New("models: invalid model id")

// ErrOverBudget means a model cannot fit inside max_disk_mb even after
// evicting everything eviction is allowed to touch.
var ErrOverBudget = errors.New("models: cannot fit")

// maxIDLen bounds an id so a hostile catalog cannot produce an unusable path.
const maxIDLen = 128

// ValidateID rejects ids that must never reach filepath.Join. Catalog entries
// arrive over the network (models.manifest_url is a remote default), so an id
// such as "../../etc/cron.d/evil" would otherwise escape Dir and let a
// compromised catalog write anywhere the daemon can.
func ValidateID(id string) error {
	if id == "" || len(id) > maxIDLen {
		return fmt.Errorf("%w: %q", ErrInvalidID, id)
	}
	// "." is allowed inside an id (version suffixes like v1.5), so ".." has to
	// be rejected explicitly — an allowlist of characters alone would pass it.
	if strings.Contains(id, "..") || id[0] == '.' || id[0] == '-' {
		return fmt.Errorf("%w: %q", ErrInvalidID, id)
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-':
		default:
			return fmt.Errorf("%w: %q", ErrInvalidID, id)
		}
	}
	return nil
}

// Manager owns <data_dir>/models: downloads, verification, pins and LRU
// eviction under the disk budget.
type Manager struct {
	Dir    string
	Client *http.Client
	Log    *slog.Logger

	// OnProgress, when set, receives byte progress during downloads
	// (throttled). total is 0 when the server sent no length and the
	// catalog carries no size.
	OnProgress func(id string, received, total int64)
	// Activity receives store events (downloads, evictions, missing
	// files). May be nil.
	Activity *activity.Ring
	// IsLoaded reports whether a model is currently loaded in a runtime;
	// retention never evicts a loaded model. Nil = nothing is loaded.
	IsLoaded func(id string) bool

	mu            sync.Mutex
	maxDiskMB     int64
	retentionDays int
	state         cacheState
	progress      map[string]Progress
	missingSeen   map[string]bool
	// reconcileRejected remembers unindexed files that failed
	// verification (keyed by id, size and mtime) so a 20 GB mismatch is
	// hashed once, not on every catalog refresh.
	reconcileRejected map[string]bool
}

// States a cache entry moves through.
const (
	StateDownloading = "downloading"
	StateReady       = "ready"
	// StateMissing is reported (never stored) for a ready entry whose file
	// is gone from disk: the index knows the model, the operator or
	// something else deleted the artifact. It does not count against the
	// budget; the next load re-downloads it.
	StateMissing = "missing"
)

// PartialMaxAge is how long an abandoned .partial download survives
// before GCPartials removes it.
const PartialMaxAge = 7 * 24 * time.Hour

// Progress is live byte progress for an in-flight download.
type Progress struct {
	ReceivedBytes int64 `json:"received_bytes"`
	TotalBytes    int64 `json:"total_bytes"`
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
	// Origin records who put the model here: "operator" (dashboard, CLI,
	// config default) or "mesh" (coordinator placement). Mesh-triggered
	// evictions may only touch mesh-origin entries; the operator's own
	// models are never the mesh's to delete. Empty = operator (pre-field
	// cache files).
	Origin string `json:"origin,omitempty"`
}

// Origins for cache entries.
const (
	OriginOperator = "operator"
	OriginMesh     = "mesh"
)

// NewManager loads (or initializes) the cache state.
func NewManager(dir string, maxDiskMB int64, log *slog.Logger) (*Manager, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("models: mkdir %s: %w", dir, err)
	}
	if log == nil {
		log = slog.Default()
	}
	m := &Manager{
		Dir:         dir,
		maxDiskMB:   maxDiskMB,
		Client:      &http.Client{}, // long downloads: no client timeout, ctx governs
		Log:         log,
		state:       cacheState{Entries: map[string]*cacheEntry{}},
		progress:    map[string]Progress{},
		missingSeen: map[string]bool{},

		reconcileRejected: map[string]bool{},
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
	// A models.json written before ValidateID existed (or tampered with) must
	// not seed the cache with ids that escape Dir when joined into a path.
	for id := range m.state.Entries {
		if err := ValidateID(id); err != nil {
			log.Warn("models: dropping cache entry with unsafe id", "id", id)
			delete(m.state.Entries, id)
		}
	}
	return m, nil
}

func (m *Manager) statePath() string { return filepath.Join(m.Dir, "models.json") }

// MaxDiskMB is the live disk budget (0 = unlimited).
func (m *Manager) MaxDiskMB() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.maxDiskMB
}

// SetMaxDiskMB changes the disk budget live (PUT /api/v1/limits). It does
// not evict immediately; the next fetch does.
func (m *Manager) SetMaxDiskMB(mb int64) {
	m.mu.Lock()
	m.maxDiskMB = max(mb, 0)
	m.mu.Unlock()
}

// RetentionDays is the live retention window (0 = never evict on age).
func (m *Manager) RetentionDays() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.retentionDays
}

// SetRetentionDays changes the retention window live; Retain applies it.
func (m *Manager) SetRetentionDays(days int) {
	m.mu.Lock()
	m.retentionDays = max(days, 0)
	m.mu.Unlock()
}

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
// partial download, or full download. Returns the local path. The entry
// is operator-owned; see EnsureOrigin.
func (m *Manager) Ensure(ctx context.Context, spec *typesv1.ModelSpec) (string, error) {
	return m.EnsureOrigin(ctx, spec, OriginOperator)
}

// EnsureOrigin is Ensure with ownership: a mesh-origin fetch may only
// evict other mesh-origin models to make room (the operator's models are
// off limits to the coordinator), and a model the operator already has is
// never re-labelled as the mesh's.
func (m *Manager) EnsureOrigin(ctx context.Context, spec *typesv1.ModelSpec, origin string) (string, error) {
	// A file:// artifact is a model the operator already has on disk (an
	// LM Studio or ollama collection, a hand-built quant). Serve it in
	// place: copying a 20GB GGUF into our cache to satisfy bookkeeping
	// would be absurd, and since we do not own the file the LRU must never
	// be allowed to delete it — so it is deliberately not registered in the
	// cache state at all.
	if err := ValidateID(spec.GetId()); err != nil {
		return "", err
	}

	if path, ok := LocalArtifactPath(spec.GetArtifactUrl()); ok {
		return m.ensureLocal(path, spec)
	}

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

	if err := m.evictForLocked(ctx, int64(spec.GetSizeBytes()), origin == OriginMesh); err != nil {
		return "", err
	}

	// The entry exists (state "downloading") for the whole transfer so the
	// local API can report it; a crash leaves it behind, and the .partial
	// file makes the next attempt resume instead of restart. The progress
	// row is registered here too, before download() opens the .partial:
	// it is what GCPartials reads to tell a live resume from an abandoned
	// one, and download() only reports its first byte count after the
	// HTTP round-trip — long enough for the hourly (or startup) GC to
	// delete an old .partial out from under the resume.
	m.mu.Lock()
	m.state.Entries[spec.GetId()] = &cacheEntry{
		ID:        spec.GetId(),
		SHA256:    spec.GetSha256(),
		SizeBytes: int64(spec.GetSizeBytes()),
		LastUsed:  time.Now(),
		State:     "downloading",
		Origin:    origin,
	}
	m.progress[spec.GetId()] = Progress{TotalBytes: int64(spec.GetSizeBytes())}
	m.saveLocked()
	m.mu.Unlock()
	actor := actorFor(origin)
	m.Activity.Record(activity.KindDownloadStarted, actor, spec.GetId(),
		fmt.Sprintf("%s started downloading %s (%s)", actor, spec.GetId(), humanBytes(int64(spec.GetSizeBytes()))), "")

	if err := m.download(ctx, spec, dest); err != nil {
		m.mu.Lock()
		delete(m.state.Entries, spec.GetId())
		delete(m.progress, spec.GetId())
		m.saveLocked()
		m.mu.Unlock()
		detail := err.Error()
		if ctx.Err() != nil {
			detail = "cancelled"
		}
		m.Activity.Record(activity.KindDownloadFailed, actor, spec.GetId(), "download of "+spec.GetId()+" failed", detail)
		return "", err
	}
	m.mu.Lock()
	delete(m.progress, spec.GetId())
	m.mu.Unlock()

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
		Origin:    origin,
	}
	delete(m.missingSeen, spec.GetId())
	m.saveLocked()
	m.mu.Unlock()
	m.Activity.Record(activity.KindDownloaded, actor, spec.GetId(),
		fmt.Sprintf("downloaded %s (%s)", spec.GetId(), humanBytes(fi.Size())), "")
	return dest, nil
}

func actorFor(origin string) string {
	if origin == OriginMesh {
		return activity.ActorMesh
	}
	return activity.ActorOperator
}

// humanBytes renders a size for activity one-liners.
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d B", n)
	}
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
	// Total: the catalog's size when known, else offset + Content-Length.
	// A truncating (200) response restarts the byte count from zero.
	if resp.StatusCode == http.StatusOK {
		offset = 0
	}
	total := int64(spec.GetSizeBytes())
	if total == 0 && resp.ContentLength > 0 {
		total = offset + resp.ContentLength
	}
	cw := &countingWriter{
		w: out,
		n: offset,
		report: func(n int64) {
			m.setProgress(spec.GetId(), n, total)
		},
	}
	cw.report(offset)
	_, cpErr := io.Copy(cw, resp.Body)
	cw.flush()
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

// countingWriter tees byte counts into a throttled progress callback so a
// 20 GB download does not turn into 20 GB of lock traffic.
type countingWriter struct {
	w      io.Writer
	n      int64
	report func(n int64)
	last   time.Time
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	if now := time.Now(); now.Sub(c.last) >= 250*time.Millisecond {
		c.last = now
		c.report(c.n)
	}
	return n, err
}

func (c *countingWriter) flush() { c.report(c.n) }

func (m *Manager) setProgress(id string, received, total int64) {
	m.mu.Lock()
	m.progress[id] = Progress{ReceivedBytes: received, TotalBytes: total}
	m.mu.Unlock()
	if m.OnProgress != nil {
		m.OnProgress(id, received, total)
	}
}

// DownloadProgress reports live byte progress for an in-flight download.
func (m *Manager) DownloadProgress(id string) (Progress, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.progress[id]
	return p, ok
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
// evictForLocked frees LRU space under the disk budget for need bytes.
// meshOnly restricts candidates to mesh-origin entries (a coordinator
// placement never costs the operator one of their own models).
func (m *Manager) evictForLocked(_ context.Context, need int64, meshOnly bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.maxDiskMB <= 0 {
		return nil // unlimited
	}
	budget := m.maxDiskMB * 1024 * 1024

	total := need
	var candidates []*cacheEntry
	for _, e := range m.state.Entries {
		if e.State == StateReady && !m.fileExistsLocked(e.ID) {
			continue // missing from disk: occupies nothing
		}
		total += e.SizeBytes
		if !e.Pinned && e.State == "ready" && (!meshOnly || e.Origin == OriginMesh) {
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
		actor := activity.ActorOperator
		if meshOnly {
			actor = activity.ActorMesh
		}
		m.Activity.Record(activity.KindEvicted, actor, e.ID,
			fmt.Sprintf("evicted %s (%s) to stay under the disk budget", e.ID, humanBytes(e.SizeBytes)), "lru, max_disk_mb")
	}
	m.saveLocked()
	if total > budget {
		return fmt.Errorf("%w: %d MB within disk budget %d MB (pinned or operator-owned models occupy the rest)", ErrOverBudget, need/1024/1024, m.maxDiskMB)
	}
	return nil
}

func (m *Manager) fileExistsLocked(id string) bool {
	_, err := os.Stat(m.Path(id))
	return err == nil
}

// Has reports whether id is on disk, complete and verified (a `cached`
// candidate: loadable without a download).
func (m *Manager) Has(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.state.Entries[id]
	return ok && e.State == StateReady && m.fileExistsLocked(id)
}

// Origin reports who owns a cached model ("" when not cached).
func (m *Manager) Origin(id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.state.Entries[id]
	if !ok {
		return ""
	}
	if e.Origin == "" {
		return OriginOperator
	}
	return e.Origin
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
	// Reachable from the local API with an operator-supplied id, and the
	// cache state on disk may predate ValidateID — so re-check before
	// os.Remove turns an id into a path.
	if err := ValidateID(id); err != nil {
		return err
	}
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
	// Origin is "operator" or "mesh" (who installed it).
	Origin string `json:"origin"`
	// ReceivedBytes is live progress, present only while downloading.
	ReceivedBytes int64 `json:"received_bytes,omitempty"`
	// Path is the artifact's absolute path; empty unless the file exists.
	Path string `json:"path,omitempty"`
}

// List returns cache contents sorted by id. Every ready entry is stat'd:
// a file deleted underneath the daemon shows as StateMissing rather than
// a phantom `ready` that still counts against the budget.
func (m *Manager) List() []Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Info, 0, len(m.state.Entries))
	var newlyMissing []string
	for _, e := range m.state.Entries {
		row := Info{ID: e.ID, SizeBytes: e.SizeBytes, Pinned: e.Pinned, LastUsed: e.LastUsed, State: e.State, Origin: e.Origin}
		if row.Origin == "" {
			row.Origin = OriginOperator
		}
		if p, ok := m.progress[e.ID]; ok {
			row.ReceivedBytes = p.ReceivedBytes
			if row.SizeBytes == 0 {
				row.SizeBytes = p.TotalBytes
			}
		}
		if e.State == StateReady {
			if m.fileExistsLocked(e.ID) {
				row.Path = m.Path(e.ID)
				delete(m.missingSeen, e.ID)
			} else {
				row.State = StateMissing
				if !m.missingSeen[e.ID] {
					m.missingSeen[e.ID] = true
					newlyMissing = append(newlyMissing, e.ID)
				}
			}
		}
		out = append(out, row)
	}
	for _, id := range newlyMissing {
		m.Log.Warn("model file missing from disk", "model", id, "path", m.Path(id))
		m.Activity.Record(activity.KindMissing, activity.ActorDaemon, id,
			id+" is missing from disk (deleted outside the daemon); it will be re-downloaded on the next load", "")
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

// DiskStats is the aggregate store view for /api/v1/status.
type DiskStats struct {
	// ModelsBytes counts complete artifacts that are actually on disk.
	ModelsBytes int64
	// PartialBytes counts resumable .partial downloads (live and abandoned).
	PartialBytes int64
	// BudgetBytes is max_disk_mb (0 = unlimited).
	BudgetBytes int64
	// FreeBytes is free space on the volume holding Dir.
	FreeBytes int64
	Dir       string
}

// Stats sizes the store: real files, not the index.
func (m *Manager) Stats() DiskStats {
	m.mu.Lock()
	st := DiskStats{Dir: m.Dir, BudgetBytes: m.maxDiskMB * 1024 * 1024}
	for _, e := range m.state.Entries {
		if e.State != StateReady {
			continue
		}
		if fi, err := os.Stat(m.Path(e.ID)); err == nil {
			st.ModelsBytes += fi.Size()
		}
	}
	m.mu.Unlock()
	for _, p := range m.partials() {
		st.PartialBytes += p.Size()
	}
	if free, err := hardware.DiskFreeBytes(m.Dir); err == nil {
		st.FreeBytes = int64(free)
	}
	return st
}

// partials lists the .partial files in Dir.
func (m *Manager) partials() []os.FileInfo {
	entries, err := os.ReadDir(m.Dir)
	if err != nil {
		return nil
	}
	var out []os.FileInfo
	for _, de := range entries {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".partial") {
			continue
		}
		if fi, err := de.Info(); err == nil {
			out = append(out, fi)
		}
	}
	return out
}

// GCPartials removes .partial files whose last write is older than maxAge
// and that no download is currently writing. An abandoned resume
// otherwise leaks its bytes forever. Returns the ids cleaned up.
func (m *Manager) GCPartials(maxAge time.Duration) []string {
	cutoff := time.Now().Add(-maxAge)
	var removed []string
	for _, fi := range m.partials() {
		id := strings.TrimSuffix(strings.TrimSuffix(fi.Name(), ".partial"), ".gguf")
		m.mu.Lock()
		_, live := m.progress[id]
		m.mu.Unlock()
		if live || fi.ModTime().After(cutoff) {
			continue
		}
		path := filepath.Join(m.Dir, fi.Name())
		if err := os.Remove(path); err != nil {
			m.Log.Warn("could not remove stale partial download", "path", path, "err", err)
			continue
		}
		m.Log.Info("removed stale partial download", "model", id, "size_mb", fi.Size()/1024/1024, "age", time.Since(fi.ModTime()).Round(time.Hour))
		removed = append(removed, id)
	}
	return removed
}

// Retain evicts unpinned, unloaded models that have not served a request
// for longer than the retention window (RetentionDays; 0 = never). Mesh
// and operator models alike: the operator opted in by setting it. Returns
// the evicted ids.
func (m *Manager) Retain() []string {
	m.mu.Lock()
	days := m.retentionDays
	if days <= 0 {
		m.mu.Unlock()
		return nil
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	var victims []*cacheEntry
	for _, e := range m.state.Entries {
		if e.Pinned || e.State != StateReady || !e.LastUsed.Before(cutoff) {
			continue
		}
		if m.IsLoaded != nil && m.IsLoaded(e.ID) {
			continue
		}
		victims = append(victims, e)
	}
	var evicted []string
	for _, e := range victims {
		if err := os.Remove(m.Path(e.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
			m.Log.Warn("retention eviction failed", "model", e.ID, "err", err)
			continue
		}
		delete(m.state.Entries, e.ID)
		evicted = append(evicted, e.ID)
		m.Log.Info("evicting model (retention)", "model", e.ID, "last_used", e.LastUsed.Format(time.RFC3339), "retention_days", days)
	}
	if len(evicted) > 0 {
		m.saveLocked()
	}
	m.mu.Unlock()
	for _, id := range evicted {
		m.Activity.Record(activity.KindEvicted, activity.ActorDaemon, id,
			fmt.Sprintf("evicted %s: unused for more than %d days", id, days), "retention_days")
	}
	return evicted
}

// Reconcile adopts artifacts that are in Dir but not in the index — a
// models.json lost or rolled back, a file copied in by hand — as
// operator-origin entries when their name is a known catalog id, their
// size matches the catalog, and (when the catalog states a real hash)
// their sha256 verifies. Hash pinning is what lets the mesh trust what a
// node serves (SPEC §6), so a file that merely has the right name and
// size is not enough; the hash is computed outside the lock, once per
// distinct file. Anything else is logged and left alone. Returns the
// adopted ids.
func (m *Manager) Reconcile(cat *Catalog) []string {
	if cat == nil {
		return nil
	}
	entries, err := os.ReadDir(m.Dir)
	if err != nil {
		return nil
	}
	type candidate struct {
		id   string
		sha  string
		size int64
		mod  time.Time
		key  string
	}
	var cands []candidate
	m.mu.Lock()
	for _, de := range entries {
		name := de.Name()
		if de.IsDir() || !strings.HasSuffix(name, ".gguf") {
			continue
		}
		id := strings.TrimSuffix(name, ".gguf")
		if _, known := m.state.Entries[id]; known {
			continue
		}
		if ValidateID(id) != nil {
			continue
		}
		fi, err := de.Info()
		if err != nil {
			continue
		}
		cm, ok := cat.Find(id)
		if !ok {
			m.Log.Debug("unindexed file in model dir is not a catalog model; leaving it", "file", name)
			continue
		}
		if cm.SizeBytes > 0 && int64(cm.SizeBytes) != fi.Size() {
			m.Log.Warn("unindexed model file has an unexpected size; not adopting it", "model", id, "size", fi.Size(), "catalog_size", cm.SizeBytes)
			continue
		}
		key := fmt.Sprintf("%s:%d:%d", id, fi.Size(), fi.ModTime().UnixNano())
		if m.reconcileRejected[key] {
			continue
		}
		cands = append(cands, candidate{id: id, sha: cm.SHA256, size: fi.Size(), mod: fi.ModTime(), key: key})
	}
	m.mu.Unlock()

	var adopted []string
	for _, c := range cands {
		if isRealSHA(c.sha) {
			start := time.Now()
			if err := verifySHA(m.Path(c.id), c.sha); err != nil {
				m.Log.Warn("unindexed model file failed verification; not adopting it", "model", c.id, "err", err)
				m.mu.Lock()
				m.reconcileRejected[c.key] = true
				m.mu.Unlock()
				continue
			}
			m.Log.Info("unindexed model file verified", "model", c.id, "took", time.Since(start).Round(time.Millisecond))
		} else {
			m.Log.Warn("adopting unindexed model file without a catalog sha256: serving unverified", "model", c.id)
		}
		m.mu.Lock()
		if _, known := m.state.Entries[c.id]; known {
			m.mu.Unlock()
			continue // a download of the same id started meanwhile
		}
		m.state.Entries[c.id] = &cacheEntry{
			ID: c.id, SHA256: c.sha, SizeBytes: c.size, LastUsed: c.mod,
			State: StateReady, Origin: OriginOperator,
		}
		m.saveLocked()
		m.mu.Unlock()
		adopted = append(adopted, c.id)
		m.Log.Info("adopted model file found in the model dir", "model", c.id, "size_mb", c.size/1024/1024)
	}
	return adopted
}

// housekeepInterval is how often RunHousekeeping runs (a var for tests).
var housekeepInterval = time.Hour

// RunHousekeeping GCs stale partials and applies retention on start and
// then hourly, until ctx ends.
func (m *Manager) RunHousekeeping(ctx context.Context) {
	tick := func() {
		m.GCPartials(PartialMaxAge)
		m.Retain()
	}
	tick()
	t := time.NewTicker(housekeepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}
