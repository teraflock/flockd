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
	"math/rand"
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
	// IsLoaded reports whether a model is in a runtime right now — loaded,
	// serving a request, or on its way in (download, admission, runtime
	// start). Neither retention nor disk-budget eviction ever takes such a
	// model: deleting the weights under a live llama-server (which mmaps
	// them) is a crash or a silently wrong answer, never a freed budget.
	// Nil = nothing is loaded.
	IsLoaded func(id string) bool

	// DownloadMaxAttempts, DownloadBackoffMin/Max bound the retry schedule
	// for a transient download failure (rate limiting, a flaky upstream).
	// NewManager sets the package defaults; tests lower them to keep a
	// simulated failure fast.
	DownloadMaxAttempts                    int
	DownloadBackoffMin, DownloadBackoffMax time.Duration

	mu            sync.Mutex
	maxDiskMB     int64
	retentionDays int
	state         cacheState
	progress      map[string]Progress
	missingSeen   map[string]bool
	// cancels holds the cancel of every download in flight, by id,
	// whoever started it (operator, mesh placement, a load of a model
	// that was not on disk). CancelDownload is the one way to stop any
	// of them; before it, only operator downloads could be cancelled.
	cancels map[string]*transfer
	// reconcileRejected remembers unindexed files that failed
	// verification (keyed by id, size and mtime) so a 20 GB mismatch is
	// hashed once, not on every catalog refresh.
	reconcileRejected map[string]bool
	// verified remembers files this process hashed successfully (keyed by
	// path, size and mtime), so a resumed multi-part download does not
	// re-hash the shards it already finished.
	verified map[string]bool
}

// transfer is a download in flight: its cancel, and done, closed once
// Ensure has returned — files closed, entry and progress settled — so a
// canceller can wait for the files to be free (Windows will not delete
// an open .partial) and for the entry to be gone before acting on them.
type transfer struct {
	cancel context.CancelFunc
	done   chan struct{}
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
	// Placement is why the mesh asked for a mesh-origin model, as the
	// coordinator said when it placed it: "floor", "demand", "warm" or
	// "stage" (proto design note 2026-11-01). Empty when unknown (an
	// operator model, or a coordinator that sends no reason).
	Placement string `json:"placement,omitempty"`
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
		Dir:                 dir,
		maxDiskMB:           maxDiskMB,
		Client:              &http.Client{}, // long downloads: no client timeout, ctx governs
		Log:                 log,
		DownloadMaxAttempts: downloadMaxAttempts,
		DownloadBackoffMin:  downloadBackoffFloor,
		DownloadBackoffMax:  downloadBackoffCeil,
		state:               cacheState{Entries: map[string]*cacheEntry{}},
		progress:            map[string]Progress{},
		missingSeen:         map[string]bool{},
		cancels:             map[string]*transfer{},

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
	// A "downloading" entry left by the previous process is not
	// downloading: nothing in this process owns it, so it would sit at 0%
	// forever, uncancellable and unremovable, while counting against the
	// disk budget. Forget the entry; the .partial on disk still makes the
	// next attempt a resume.
	for id, e := range m.state.Entries {
		if e.State == StateDownloading {
			log.Info("models: forgetting download interrupted by the previous run", "model", id)
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
		switch {
		case hashMoved(e, spec):
			// The catalog now pins a different artifact under this id
			// (an upstream re-upload: seven entries moved under us in
			// 2026-10). The file on disk is the old one: Verify() would
			// reject it, and its outputs would not match the fingerprints
			// the mesh keys by the new hash (SPEC §6). Drop it and fetch
			// what the manifest pins; the download verifies as always.
			m.Log.Warn("manifest hash changed; refetching", "model", id,
				"had", e.SHA256, "want", spec.GetSha256())
			if err := m.removeFilesLocked(e); err != nil {
				m.Log.Warn("could not remove superseded artifact", "model", id, "err", err)
			}
		case m.filesExistLocked(e):
			// Trust-but-verify on startup use: cheap stat; full hash was
			// done at download time. Serving re-verification happens in
			// Verify().
			a := m.artifact(e)
			m.mu.Unlock()
			return a, nil
		}
		// File vanished under us, or is superseded: fall through to
		// re-download.
		delete(m.state.Entries, id)
		m.saveLocked()
	}
	m.mu.Unlock()

	// Pre-flight on the whole set: nothing is fetched that cannot fit.
	need := set.totalBytes()
	if err := m.evictForLocked(ctx, id, need, origin == OriginMesh); err != nil {
		// The operator sees this row; the 202 that started an operator
		// download went out before this check ran.
		m.Activity.Record(activity.KindDownloadFailed, actorFor(origin), id,
			"download of "+id+" failed", err.Error())
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

	dlCtx, cancelDL := context.WithCancel(ctx)
	tr := &transfer{cancel: cancelDL, done: make(chan struct{})}
	m.mu.Lock()
	m.cancels[id] = tr
	m.mu.Unlock()
	defer close(tr.done) // after every return path below has settled the entry
	err = m.downloadSet(dlCtx, set, entry)
	m.mu.Lock()
	delete(m.cancels, id)
	m.mu.Unlock()
	cancelDL()
	if err != nil {
		m.mu.Lock()
		delete(m.state.Entries, id)
		delete(m.progress, id)
		m.saveLocked()
		m.mu.Unlock()
		detail := err.Error()
		if dlCtx.Err() != nil {
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

// hashMoved reports whether the manifest pins a different artifact than
// the cached entry holds. Placeholder hashes on either side (drafts, or
// pre-field cache entries) never count as a move: there is nothing real
// to disagree about.
func hashMoved(e *cacheEntry, spec *typesv1.ModelSpec) bool {
	want := spec.GetSha256()
	return isRealSHA(want) && isRealSHA(e.SHA256) && e.SHA256 != want
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

// downloadMaxAttempts, downloadBackoffFloor/Ceil bound the retry schedule for
// a transient download failure (rate limiting, a flaky upstream) — hosts
// like Hugging Face, which most catalog artifact_urls point at directly,
// rate-limit anonymous fetches (429) with no auth option here to avoid it.
const (
	downloadMaxAttempts  = 6
	downloadBackoffFloor = 2 * time.Second
	downloadBackoffCeil  = 60 * time.Second
)

// transientDownloadError marks a download failure as worth retrying — a
// 429/5xx from the upstream host or a transport-level error — as opposed
// to one a retry can't fix (404, a bad URL, a hash mismatch).
type transientDownloadError struct {
	err error
	// retryAfter, when nonzero, is the upstream's requested wait (the
	// Retry-After header) and overrides the caller's own backoff.
	retryAfter time.Duration
}

func (e *transientDownloadError) Error() string { return e.err.Error() }
func (e *transientDownloadError) Unwrap() error { return e.err }

func isRetryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

// parseRetryAfter reads a Retry-After header (either delta-seconds or an
// HTTP-date); 0 means absent, invalid, or already in the past.
func parseRetryAfter(h string) time.Duration {
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// jitter returns d +/- 30%, so retries from several files/models don't
// all land on the same upstream request in lockstep.
func jitter(d time.Duration) time.Duration {
	f := 0.7 + 0.6*rand.Float64() //nolint:gosec // backoff jitter, not crypto
	return time.Duration(float64(d) * f)
}

// download performs a resumable fetch of one file into dest, retrying a
// transient failure (upstream rate limit, a 5xx, a transport error) with
// backoff instead of failing the whole pull on one bad request.
func (m *Manager) download(ctx context.Context, id string, f artifactFile, dest string, base, total int64) error {
	maxAttempts := max(m.DownloadMaxAttempts, 1)
	backoff := m.DownloadBackoffMin
	if backoff <= 0 {
		backoff = downloadBackoffFloor
	}
	backoffMax := m.DownloadBackoffMax
	if backoffMax <= 0 {
		backoffMax = downloadBackoffCeil
	}
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err = m.downloadAttempt(ctx, id, f, dest, base, total)
		if err == nil {
			return nil
		}
		var te *transientDownloadError
		if !errors.As(err, &te) || attempt == maxAttempts {
			break
		}
		wait := te.retryAfter
		if wait <= 0 {
			wait = jitter(backoff)
			backoff = min(backoff*2, backoffMax)
		}
		m.Log.Warn("model download failed transiently; retrying", "model", id, "file", f.Name, "err", err, "wait", wait, "attempt", attempt)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	return err
}

// downloadAttempt performs one fetch of one file into dest via a .partial
// temp file, verifying SHA256 over the complete content before renaming
// into place. base is what the model's earlier files already add up to
// and total the whole set, so progress is reported for the model, not the
// file. Errors worth retrying are wrapped in *transientDownloadError.
func (m *Manager) downloadAttempt(ctx context.Context, id string, f artifactFile, dest string, base, total int64) error {
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
		wrapped := fmt.Errorf("models: download %s: %w", id, err)
		if ctx.Err() != nil {
			return wrapped // cancelled/deadline, not a transient upstream hiccup
		}
		return &transientDownloadError{err: wrapped}
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
		statusErr := fmt.Errorf("models: download %s (%s): status %s", id, f.Name, resp.Status)
		if isRetryableStatus(resp.StatusCode) {
			return &transientDownloadError{err: statusErr, retryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
		}
		return statusErr
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

// evictForLocked frees LRU space under the disk budget so a new artifact
// of size need (the model forID) fits inside MaxDiskMB, deleting
// least-recently-used models first. Never a candidate: a pinned model, a
// model that is still downloading, a model IsLoaded says is in a runtime
// (loaded, serving, or being loaded — the weights are mmapped by a live
// process), and, when meshOnly, an operator-owned model (a coordinator
// placement never costs the operator one of their own). Every eviction
// leaves an activity row naming the model it made room for, and when the
// artifact cannot fit the download_failed row says what stood in the way.
func (m *Manager) evictForLocked(_ context.Context, forID string, need int64, meshOnly bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.maxDiskMB <= 0 {
		return nil // unlimited
	}
	budget := m.maxDiskMB * 1024 * 1024

	total := need
	var candidates []*cacheEntry
	var skippedLoaded, skippedPinned, skippedOperator []string
	for _, e := range m.state.Entries {
		if e.State == StateReady && !m.filesExistLocked(e) {
			continue // missing from disk: occupies nothing
		}
		total += e.SizeBytes
		if e.State != StateReady {
			continue
		}
		switch {
		case e.Pinned:
			skippedPinned = append(skippedPinned, e.ID)
		case meshOnly && e.Origin != OriginMesh:
			skippedOperator = append(skippedOperator, e.ID)
		case m.IsLoaded != nil && m.IsLoaded(e.ID):
			skippedLoaded = append(skippedLoaded, e.ID)
		default:
			candidates = append(candidates, e)
		}
	}
	if total <= budget {
		return nil
	}
	sort.Strings(skippedLoaded)
	sort.Strings(skippedPinned)
	sort.Strings(skippedOperator)
	why := evictionBlockers(skippedLoaded, skippedPinned, skippedOperator)
	// Decide before deleting anything: when evicting every candidate still
	// would not make room, the artifact does not fit and nothing is
	// evicted. The old order (evict first, check after) let a placement
	// too big for the budget wipe the node's mesh models and then be
	// declined anyway — and the coordinator re-placed what was wiped.
	var reclaimable int64
	for _, e := range candidates {
		reclaimable += e.SizeBytes
	}
	if total-reclaimable > budget {
		return fmt.Errorf("%w: %d MB within disk budget %d MB (%s)", ErrOverBudget, need/1024/1024, m.maxDiskMB, why)
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].LastUsed.Before(candidates[j].LastUsed)
	})
	actor := activity.ActorOperator
	if meshOnly {
		actor = activity.ActorMesh
	}
	detail := "lru, max_disk_mb"
	if len(skippedLoaded) > 0 {
		detail += "; kept (loaded): " + strings.Join(skippedLoaded, ", ")
	}
	for _, e := range candidates {
		if total <= budget {
			break
		}
		m.Log.Info("evicting model (LRU, disk budget)", "model", e.ID, "size_mb", e.SizeBytes/1024/1024, "for", forID, "kept_loaded", skippedLoaded)
		if err := m.removeFilesLocked(e); err != nil {
			return fmt.Errorf("models: evict %s: %w", e.ID, err)
		}
		total -= e.SizeBytes
		delete(m.state.Entries, e.ID)
		m.Activity.Record(activity.KindEvicted, actor, e.ID,
			fmt.Sprintf("evicted %s (%s) to make room for %s under the disk budget", e.ID, humanBytes(e.SizeBytes), forID), detail)
	}
	m.saveLocked()
	if total > budget {
		return fmt.Errorf("%w: %d MB within disk budget %d MB (%s)", ErrOverBudget, need/1024/1024, m.maxDiskMB, why)
	}
	return nil
}

// evictionBlockers words what kept the budget from being freed, for the
// ErrOverBudget message and the download_failed activity row: which
// models were not candidates and why. Loaded models are named first —
// before this, "pinned or operator-owned models occupy the rest" was all
// the operator saw when the real reason was a model still serving.
func evictionBlockers(loaded, pinned, operator []string) string {
	var parts []string
	if len(loaded) > 0 {
		parts = append(parts, "loaded: "+strings.Join(loaded, ", "))
	}
	if len(pinned) > 0 {
		parts = append(parts, "pinned: "+strings.Join(pinned, ", "))
	}
	if len(operator) > 0 {
		parts = append(parts, "operator-owned: "+strings.Join(operator, ", "))
	}
	if len(parts) == 0 {
		return "nothing evictable"
	}
	return "kept " + strings.Join(parts, "; ")
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

// Placement reports why the mesh placed a cached model ("" when unknown
// or not cached); see cacheEntry.Placement.
func (m *Manager) Placement(id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.state.Entries[id]; ok {
		return e.Placement
	}
	return ""
}

// SetPlacement records why the mesh asked for a cached model, replacing
// an earlier reason (a warm of a cached copy is a new placement). A model
// the cache does not know is ignored.
func (m *Manager) SetPlacement(id, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.state.Entries[id]
	if !ok || e.Placement == reason {
		return
	}
	e.Placement = reason
	m.saveLocked()
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
	// A download in flight for the id is stopped first and waited for:
	// otherwise its goroutine would re-add the entry as ready when it
	// finished, undoing the removal, and its open .partial could not be
	// deleted on Windows. A cancelled transfer has already dropped its
	// own entry, so what is left to remove is the resume state.
	cancelled := m.CancelDownload(id)
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.state.Entries[id]
	if !ok {
		if !cancelled {
			return fmt.Errorf("models: %s not in cache", id)
		}
		m.removeResumeStateLocked(id)
		return nil
	}
	if err := m.removeFilesLocked(e); err != nil {
		return fmt.Errorf("models: remove %s: %w", id, err)
	}
	m.removeResumeStateLocked(id)
	delete(m.state.Entries, id)
	delete(m.progress, id)
	m.saveLocked()
	return nil
}

// removeResumeStateLocked deletes what a cancelled or finished download
// leaves for a resume: the single-file .partial, or a multi-file set's
// directory (which holds its per-file partials). Remove means gone,
// resume included.
func (m *Manager) removeResumeStateLocked(id string) {
	_ = os.Remove(filepath.Join(m.Dir, id+".gguf.partial"))
	_ = os.RemoveAll(filepath.Join(m.Dir, id))
}

// CancelDownload stops the download of id whoever started it, waits for
// it to wind down (files closed, entry gone), and reports whether there
// was one. The .partial stays for a later resume; Remove deletes it.
// Never call it with m.mu held: the transfer needs the lock to finish.
func (m *Manager) CancelDownload(id string) bool {
	m.mu.Lock()
	tr, ok := m.cancels[id]
	m.mu.Unlock()
	if !ok {
		return false
	}
	tr.cancel()
	<-tr.done
	return true
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
	// Placement is why the mesh placed it (floor, demand, warm, stage);
	// "" when unknown.
	Placement string `json:"placement,omitempty"`
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
		row := Info{ID: e.ID, SizeBytes: e.SizeBytes, Pinned: e.Pinned, LastUsed: e.LastUsed, State: e.State, Origin: e.Origin, Placement: e.Placement}
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
