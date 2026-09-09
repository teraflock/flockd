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
	// verified remembers files this process hashed successfully (keyed by
	// path, size and mtime), so a resumed multi-part download does not
	// re-hash the shards it already finished.
	verified map[string]bool
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
	// Parts and Mmproj are set for a multi-file artifact (sharded GGUF
	// and/or vision projector), which lives in <Dir>/<id>/ under the
	// upstream basenames; SHA256 is then the composite id, not a file
	// hash, and each file carries its own. Absent for a plain <id>.gguf.
	Parts  []partEntry `json:"parts,omitempty"`
	Mmproj *partEntry  `json:"mmproj,omitempty"`
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
		verified:          map[string]bool{},
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

// Path returns where a model artifact lives locally: <Dir>/<id>.gguf, or
// for a multi-file entry the first shard inside <Dir>/<id>/ (llama-server
// discovers the sibling shards from it). An id the cache does not know is
// assumed single-file.
func (m *Manager) Path(id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pathLocked(id)
}

func (m *Manager) pathLocked(id string) string {
	if e, ok := m.state.Entries[id]; ok && e.multi() {
		return m.artifact(e).Path
	}
	return filepath.Join(m.Dir, id+".gguf")
}

// Artifact is a model's files as the runtime needs them.
type Artifact struct {
	// Path is the GGUF to load: the file, or part 1 of a sharded set.
	Path string
	// MmprojPath is the vision projector sidecar for --mmproj; "" when none.
	MmprojPath string
	// SizeBytes is what the whole set occupies on disk (0 = unknown).
	SizeBytes int64
}

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
	a, err := m.EnsureArtifact(ctx, spec, origin)
	return a.Path, err
}

// EnsureArtifact is EnsureOrigin returning every path the runtime needs
// (the mmproj sidecar has no place in a single string).
//
// A sharded spec (parts) is fetched one part at a time, each through the
// same resumable, hash-verified download as a single file, into
// <Dir>/<id>/ under the upstream names; a shard that verified once is
// never fetched again, and progress is the sum over the whole set.
func (m *Manager) EnsureArtifact(ctx context.Context, spec *typesv1.ModelSpec, origin string) (Artifact, error) {
	// A file:// artifact is a model the operator already has on disk (an
	// LM Studio or ollama collection, a hand-built quant). Serve it in
	// place: copying a 20GB GGUF into our cache to satisfy bookkeeping
	// would be absurd, and since we do not own the file the LRU must never
	// be allowed to delete it — so it is deliberately not registered in the
	// cache state at all.
	id := spec.GetId()
	if err := ValidateID(id); err != nil {
		return Artifact{}, err
	}
	if spec.GetArtifactUrl() == "" && len(spec.GetParts()) == 0 {
		return Artifact{}, fmt.Errorf("models: %s: %w", id, ErrNoArtifact)
	}
	if isLocalSpec(spec) {
		return m.ensureLocalSet(spec)
	}
	set, err := m.artifactSet(spec)
	if err != nil {
		return Artifact{}, err
	}

	m.mu.Lock()
	if e, ok := m.state.Entries[id]; ok && e.State == StateReady {
		e.LastUsed = time.Now()
		m.saveLocked()
		// Trust-but-verify on startup use: cheap stat; full hash was done at
		// download time. Serving re-verification happens in Verify().
		if m.filesExistLocked(e) {
			a := m.artifact(e)
			m.mu.Unlock()
			return a, nil
		}
		// File vanished under us: fall through to re-download.
		delete(m.state.Entries, id)
		m.saveLocked()
	}
	m.mu.Unlock()

	// Pre-flight on the whole set: nothing is fetched that cannot fit.
	need := set.totalBytes()
	if err := m.evictForLocked(ctx, need, origin == OriginMesh); err != nil {
		return Artifact{}, err
	}

	// The entry exists (state "downloading") for the whole transfer so the
	// local API can report it; a crash leaves it behind, and the .partial
	// file makes the next attempt resume instead of restart. The progress
	// row is registered here too, before download() opens the .partial:
	// it is what GCPartials reads to tell a live resume from an abandoned
	// one, and download() only reports its first byte count after the
	// HTTP round-trip — long enough for the hourly (or startup) GC to
	// delete an old .partial out from under the resume.
	entry := set.entry(spec.GetSha256(), origin)
	entry.SizeBytes = need
	entry.LastUsed = time.Now()
	entry.State = StateDownloading
	m.mu.Lock()
	m.state.Entries[id] = entry
	m.progress[id] = Progress{TotalBytes: need}
	m.saveLocked()
	m.mu.Unlock()
	actor := actorFor(origin)
	m.Activity.Record(activity.KindDownloadStarted, actor, id,
		fmt.Sprintf("%s started downloading %s (%s)", actor, id, humanBytes(need)), "")

	if err := m.downloadSet(ctx, set, entry); err != nil {
		m.mu.Lock()
		delete(m.state.Entries, id)
		delete(m.progress, id)
		m.saveLocked()
		m.mu.Unlock()
		detail := err.Error()
		if ctx.Err() != nil {
			detail = "cancelled"
		}
		m.Activity.Record(activity.KindDownloadFailed, actor, id, "download of "+id+" failed", detail)
		return Artifact{}, err
	}
	m.mu.Lock()
	delete(m.progress, id)
	m.mu.Unlock()

	var size int64
	for _, f := range m.entryFiles(entry) {
		fi, err := os.Stat(f.Path)
		if err != nil {
			return Artifact{}, fmt.Errorf("models: stat %s: %w", f.Path, err)
		}
		size += fi.Size()
	}
	m.mu.Lock()
	entry.SizeBytes = size
	entry.LastUsed = time.Now()
	entry.State = StateReady
	m.state.Entries[id] = entry
	delete(m.missingSeen, id)
	m.saveLocked()
	a := m.artifact(entry)
	m.mu.Unlock()
	m.Activity.Record(activity.KindDownloaded, actor, id,
		fmt.Sprintf("downloaded %s (%s)", id, humanBytes(size)), "")
	return a, nil
}

// downloadSet fetches the set's files in order. A file already in place
// with the right size and hash (a shard finished before a crash or a
// cancel, or copied in by hand) is kept; anything else goes through
// download(), whose .partial is per file, so a failed shard costs only
// its own progress.
func (m *Manager) downloadSet(ctx context.Context, set artifactSet, entry *cacheEntry) error {
	if set.Multi {
		if err := os.MkdirAll(set.Dir, 0o755); err != nil {
			return fmt.Errorf("models: mkdir %s: %w", set.Dir, err)
		}
	}
	total := set.totalBytes()
	var done int64
	for i, f := range set.Files {
		dest := filepath.Join(set.Dir, f.Name)
		if !m.haveVerified(dest, f) {
			if err := m.download(ctx, set.ID, f, dest, done, total); err != nil {
				return err
			}
		}
		fi, err := os.Stat(dest)
		if err != nil {
			return fmt.Errorf("models: stat %s: %w", dest, err)
		}
		done += fi.Size()
		m.mu.Lock()
		if files := entry.files(); i < len(files) {
			files[i].State = StateReady
			m.saveLocked()
		}
		m.mu.Unlock()
		m.setProgress(set.ID, done, total)
	}
	return nil
}

// haveVerified reports whether dest already holds f: the expected size and,
// unless this process hashed that very file before, the expected hash. A
// wrong file is left for download() to overwrite.
func (m *Manager) haveVerified(dest string, f artifactFile) bool {
	fi, err := os.Stat(dest)
	if err != nil || fi.IsDir() || (f.SizeBytes > 0 && fi.Size() != f.SizeBytes) {
		return false
	}
	key := verifiedKey(dest, fi)
	m.mu.Lock()
	ok := m.verified[key]
	m.mu.Unlock()
	if ok {
		return true
	}
	if !isRealSHA(f.SHA256) || verifySHA(dest, f.SHA256) != nil {
		return false
	}
	m.rememberVerified(dest)
	return true
}

func verifiedKey(path string, fi os.FileInfo) string {
	return fmt.Sprintf("%s:%d:%d", path, fi.Size(), fi.ModTime().UnixNano())
}

func (m *Manager) rememberVerified(path string) {
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	m.mu.Lock()
	m.verified[verifiedKey(path, fi)] = true
	m.mu.Unlock()
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

// download performs a resumable fetch of one file into dest via a
// .partial temp file, verifying SHA256 over the complete content before
// renaming into place. base is what the model's earlier files already
// add up to and total the whole set, so progress is reported for the
// model, not the file.
func (m *Manager) download(ctx context.Context, id string, f artifactFile, dest string, base, total int64) error {
	tmp := dest + ".partial"
	var offset int64
	if fi, err := os.Stat(tmp); err == nil {
		offset = fi.Size()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return fmt.Errorf("models: request %s: %w", f.URL, err)
	}
	if offset > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
	}
	resp, err := m.Client.Do(req)
	if err != nil {
		return fmt.Errorf("models: download %s: %w", id, err)
	}
	defer resp.Body.Close()

	flags := os.O_CREATE | os.O_WRONLY
	switch resp.StatusCode {
	case http.StatusPartialContent:
		flags |= os.O_APPEND
		m.Log.Info("resuming model download", "model", id, "file", f.Name, "offset", offset)
	case http.StatusOK:
		flags |= os.O_TRUNC // server ignored Range: start over
	default:
		return fmt.Errorf("models: download %s (%s): status %s", id, f.Name, resp.Status)
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
	if total == 0 && resp.ContentLength > 0 {
		total = base + offset + resp.ContentLength
	}
	cw := &countingWriter{
		w: out,
		n: offset,
		report: func(n int64) {
			m.setProgress(id, base+n, total)
		},
	}
	cw.report(offset)
	_, cpErr := io.Copy(cw, resp.Body)
	cw.flush()
	closeErr := out.Close()
	if cpErr != nil {
		return fmt.Errorf("models: write %s: %w", id, cpErr) // .partial kept for resume
	}
	if closeErr != nil {
		return fmt.Errorf("models: close %s: %w", tmp, closeErr)
	}

	if err := verifySHA(tmp, f.SHA256); err != nil {
		_ = os.Remove(tmp) // poisoned: do not resume garbage
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return fmt.Errorf("models: finalize %s: %w", dest, err)
	}
	m.rememberVerified(dest)
	m.Log.Info("model file downloaded and verified", "model", id, "file", f.Name, "sha256", f.SHA256)
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

// Verify re-hashes a cached artifact against its recorded SHA — every
// shard and the mmproj against their own hash for a multi-file entry;
// the composite id is never checked against a file. Serving paths call
// this before loading a model into the runtime.
func (m *Manager) Verify(id string) error {
	m.mu.Lock()
	e, ok := m.state.Entries[id]
	var files []fileRef
	if ok {
		files = m.entryFiles(e)
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("models: %s not in cache", id)
	}
	for _, f := range files {
		if err := verifySHA(f.Path, f.SHA256); err != nil {
			return err
		}
	}
	return nil
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
		if e.State == StateReady && !m.filesExistLocked(e) {
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
		if err := m.removeFilesLocked(e); err != nil {
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

// filesExistLocked reports whether every file of the entry is on disk.
func (m *Manager) filesExistLocked(e *cacheEntry) bool {
	for _, f := range m.entryFiles(e) {
		if _, err := os.Stat(f.Path); err != nil {
			return false
		}
	}
	return true
}

// removeFilesLocked deletes an entry's files: the one GGUF, or the whole
// <Dir>/<id>/ directory of a multi-file set (the id was validated when the
// entry was created, so the directory is ours). A file already gone is
// not an error.
func (m *Manager) removeFilesLocked(e *cacheEntry) error {
	if e.multi() {
		return os.RemoveAll(filepath.Join(m.Dir, e.ID))
	}
	if err := os.Remove(filepath.Join(m.Dir, e.ID+".gguf")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Has reports whether id is on disk, complete and verified (a `cached`
// candidate: loadable without a download).
func (m *Manager) Has(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.state.Entries[id]
	return ok && e.State == StateReady && m.filesExistLocked(e)
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
	e, ok := m.state.Entries[id]
	if !ok {
		return fmt.Errorf("models: %s not in cache", id)
	}
	if err := m.removeFilesLocked(e); err != nil {
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
	// For a multi-file entry it is the first shard (see Manager.Path).
	Path string `json:"path,omitempty"`
	// PartsTotal is the number of files in a multi-file artifact (shards
	// plus the mmproj sidecar); 0 for a plain single-file model.
	PartsTotal int `json:"parts_total,omitempty"`
	// PartsDone is how many of them are verified and in place; equal to
	// PartsTotal once ready.
	PartsDone int `json:"parts_done,omitempty"`
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
		if e.multi() {
			row.PartsTotal = len(e.files())
			row.PartsDone = e.partsDone()
		}
		if e.State == StateReady {
			if m.filesExistLocked(e) {
				row.Path = m.pathLocked(e.ID)
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
		m.Log.Warn("model file missing from disk", "model", id, "path", m.pathLocked(id))
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
		for _, f := range m.entryFiles(e) {
			if fi, err := os.Stat(f.Path); err == nil {
				st.ModelsBytes += fi.Size()
			}
		}
	}
	m.mu.Unlock()
	for _, p := range m.partials() {
		st.PartialBytes += p.info.Size()
	}
	if free, err := hardware.DiskFreeBytes(m.Dir); err == nil {
		st.FreeBytes = int64(free)
	}
	return st
}

// partial is a resumable download file: <id>.gguf.partial in Dir, or
// <file>.partial inside a multi-file entry's <Dir>/<id>/.
type partial struct {
	id   string
	path string
	info os.FileInfo
}

// partials lists the .partial files in Dir and one level below it.
func (m *Manager) partials() []partial {
	entries, err := os.ReadDir(m.Dir)
	if err != nil {
		return nil
	}
	var out []partial
	for _, de := range entries {
		name := de.Name()
		switch {
		case de.IsDir():
			if ValidateID(name) != nil {
				continue
			}
			sub, err := os.ReadDir(filepath.Join(m.Dir, name))
			if err != nil {
				continue
			}
			for _, sd := range sub {
				if sd.IsDir() || !strings.HasSuffix(sd.Name(), ".partial") {
					continue
				}
				if fi, err := sd.Info(); err == nil {
					out = append(out, partial{id: name, path: filepath.Join(m.Dir, name, sd.Name()), info: fi})
				}
			}
		case strings.HasSuffix(name, ".partial"):
			if fi, err := de.Info(); err == nil {
				id := strings.TrimSuffix(strings.TrimSuffix(name, ".partial"), ".gguf")
				out = append(out, partial{id: id, path: filepath.Join(m.Dir, name), info: fi})
			}
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
	for _, p := range m.partials() {
		m.mu.Lock()
		_, live := m.progress[p.id]
		m.mu.Unlock()
		if live || p.info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(p.path); err != nil {
			m.Log.Warn("could not remove stale partial download", "path", p.path, "err", err)
			continue
		}
		m.Log.Info("removed stale partial download", "model", p.id, "file", p.info.Name(), "size_mb", p.info.Size()/1024/1024, "age", time.Since(p.info.ModTime()).Round(time.Hour))
		removed = append(removed, p.id)
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
		if err := m.removeFilesLocked(e); err != nil {
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
// distinct file. A multi-file set (<id>/ holding the catalog's shards and
// mmproj) is adopted only when every file is present and verifies.
// Anything else is logged and left alone. Returns the adopted ids.
func (m *Manager) Reconcile(cat *Catalog) []string {
	if cat == nil {
		return nil
	}
	entries, err := os.ReadDir(m.Dir)
	if err != nil {
		return nil
	}
	type candidate struct {
		entry *cacheEntry
		files []fileRef
		key   string
	}
	var cands []candidate
	m.mu.Lock()
	for _, de := range entries {
		name := de.Name()
		id := strings.TrimSuffix(name, ".gguf")
		if !de.IsDir() && id == name {
			continue
		}
		if _, known := m.state.Entries[id]; known {
			continue
		}
		if ValidateID(id) != nil {
			continue
		}
		cm, ok := cat.Find(id)
		if !ok {
			m.Log.Debug("unindexed file in model dir is not a catalog model; leaving it", "file", name)
			continue
		}
		set, err := m.artifactSet(cm.Spec())
		if err != nil {
			m.Log.Warn("unindexed model file has an unusable catalog entry; not adopting it", "model", id, "err", err)
			continue
		}
		if set.Multi != de.IsDir() {
			m.Log.Warn("unindexed model file does not match the catalog's layout; not adopting it", "model", id, "multi_part", set.Multi)
			continue
		}
		e := set.entry(cm.SHA256, OriginOperator)
		files := m.entryFiles(e)
		var size int64
		var mod time.Time
		complete := true
		for i, f := range files {
			fi, err := os.Stat(f.Path)
			if err != nil || fi.IsDir() {
				complete = false
				break
			}
			if want := set.Files[i].SizeBytes; want > 0 && fi.Size() != want {
				m.Log.Warn("unindexed model file has an unexpected size; not adopting it", "model", id, "file", filepath.Base(f.Path), "size", fi.Size(), "catalog_size", want)
				complete = false
				break
			}
			size += fi.Size()
			if fi.ModTime().After(mod) {
				mod = fi.ModTime()
			}
		}
		if !complete {
			continue
		}
		key := fmt.Sprintf("%s:%d:%d", id, size, mod.UnixNano())
		if m.reconcileRejected[key] {
			continue
		}
		e.SizeBytes, e.LastUsed, e.State = size, mod, StateReady
		for _, f := range e.files() {
			f.State = StateReady
		}
		cands = append(cands, candidate{entry: e, files: files, key: key})
	}
	m.mu.Unlock()

	var adopted []string
	for _, c := range cands {
		id := c.entry.ID
		verified := true
		for _, f := range c.files {
			if !isRealSHA(f.SHA256) {
				m.Log.Warn("adopting unindexed model file without a catalog sha256: serving unverified", "model", id, "file", filepath.Base(f.Path))
				continue
			}
			start := time.Now()
			if err := verifySHA(f.Path, f.SHA256); err != nil {
				m.Log.Warn("unindexed model file failed verification; not adopting it", "model", id, "err", err)
				verified = false
				break
			}
			m.Log.Info("unindexed model file verified", "model", id, "file", filepath.Base(f.Path), "took", time.Since(start).Round(time.Millisecond))
		}
		if !verified {
			m.mu.Lock()
			m.reconcileRejected[c.key] = true
			m.mu.Unlock()
			continue
		}
		m.mu.Lock()
		if _, known := m.state.Entries[id]; known {
			m.mu.Unlock()
			continue // a download of the same id started meanwhile
		}
		m.state.Entries[id] = c.entry
		m.saveLocked()
		m.mu.Unlock()
		adopted = append(adopted, id)
		m.Log.Info("adopted model file found in the model dir", "model", id, "files", len(c.files), "size_mb", c.entry.SizeBytes/1024/1024)
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
