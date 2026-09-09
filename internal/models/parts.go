package models

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"strings"

	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
)

// ErrNoArtifact means a ModelSpec names neither an artifact_url nor parts:
// there is nothing to download. A daemon that predates multi-part specs
// sees every sharded assignment this way, and reports it `failed` with
// this reason so the coordinator backs off (proto design note
// 2026-09-08-sharded-artifacts).
var ErrNoArtifact = errors.New("no artifact in spec")

// CompositeSHA256 is the single "quant sha" of a multi-part artifact: the
// hex sha256 over the concatenation of the lowercase-hex part hashes in
// series order, no separators. The catalog emitter, the control-plane
// registry and the daemon all derive the same value, so a sharded spec's
// sha256 keeps meaning for DispatchRequest.quant_sha256 pinning. It is
// never a file hash: files are verified against their own part hash.
func CompositeSHA256(partHashes []string) string {
	h := sha256.New()
	for _, p := range partHashes {
		_, _ = h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// artifactFile is one file of a model's artifact set as the store lays it
// out: its basename on disk, where it comes from, what it must hash to.
type artifactFile struct {
	Name      string
	URL       string
	SHA256    string
	SizeBytes int64
	Mmproj    bool
}

// artifactSet is what a ModelSpec resolves to on disk.
//
// A plain single-file model is <Dir>/<id>.gguf, exactly as before parts
// existed. Anything with more than one file — a sharded GGUF, or a model
// with an mmproj sidecar — lives in its own <Dir>/<id>/ directory under
// the upstream basenames: llama-server derives the sibling shard names
// from the "-NNNNN-of-NNNNN" pattern of the file it is given, so the
// names must be preserved, and a subdirectory keeps Reconcile's flat
// *.gguf scan unambiguous.
type artifactSet struct {
	ID    string
	Dir   string
	Files []artifactFile
	Multi bool
}

// totalBytes is what the whole set will occupy: the catalog's sizes,
// mmproj included — it is real disk.
func (a artifactSet) totalBytes() int64 {
	var n int64
	for _, f := range a.Files {
		n += f.SizeBytes
	}
	return n
}

// entry is the cache record for the set at the start of a download.
func (a artifactSet) entry(sha, origin string) *cacheEntry {
	e := &cacheEntry{ID: a.ID, SHA256: sha, Origin: origin}
	if !a.Multi {
		return e
	}
	for _, f := range a.Files {
		p := partEntry{Name: f.Name, SHA256: f.SHA256, SizeBytes: f.SizeBytes}
		if f.Mmproj {
			e.Mmproj = &p
		} else {
			e.Parts = append(e.Parts, p)
		}
	}
	return e
}

// artifactSet resolves a spec's files and layout. The id must already be
// validated. Whether there is anything to fetch is the caller's check
// (ErrNoArtifact): Reconcile only needs the layout.
func (m *Manager) artifactSet(spec *typesv1.ModelSpec) (artifactSet, error) {
	id := spec.GetId()
	parts, mmproj := spec.GetParts(), spec.GetMmproj()
	if len(parts) == 0 && mmproj == nil {
		return artifactSet{ID: id, Dir: m.Dir, Files: []artifactFile{{
			Name: id + ".gguf", URL: spec.GetArtifactUrl(), SHA256: spec.GetSha256(), SizeBytes: int64(spec.GetSizeBytes()),
		}}}, nil
	}

	set := artifactSet{ID: id, Dir: filepath.Join(m.Dir, id), Multi: true}
	if len(parts) == 0 {
		// One GGUF plus its projector: the main file keeps its upstream
		// name too, falling back to <id>.gguf when the URL has none.
		name, err := urlBasename(spec.GetArtifactUrl())
		if err != nil {
			name = id + ".gguf"
		}
		set.Files = append(set.Files, artifactFile{
			Name: name, URL: spec.GetArtifactUrl(), SHA256: spec.GetSha256(), SizeBytes: int64(spec.GetSizeBytes()),
		})
	}
	var hashes []string
	allReal := true
	for i, p := range parts {
		name, err := urlBasename(p.GetUrl())
		if err != nil {
			return artifactSet{}, fmt.Errorf("models: %s: part %d: %w", id, i+1, err)
		}
		set.Files = append(set.Files, artifactFile{Name: name, URL: p.GetUrl(), SHA256: p.GetSha256(), SizeBytes: int64(p.GetSizeBytes())})
		hashes = append(hashes, p.GetSha256())
		allReal = allReal && isRealSHA(p.GetSha256())
	}
	if len(parts) > 0 && allReal && spec.GetSha256() != CompositeSHA256(hashes) {
		// The catalog disagrees with itself: the composite is derived from
		// the part hashes, so a mismatch means one side was edited by hand.
		// Hash pinning (SPEC §6) is not something to guess around.
		return artifactSet{}, fmt.Errorf("models: %s: sha256 %s is not the composite of its %d parts", id, spec.GetSha256(), len(parts))
	}
	if mmproj != nil {
		name, err := urlBasename(mmproj.GetUrl())
		if err != nil {
			return artifactSet{}, fmt.Errorf("models: %s: mmproj: %w", id, err)
		}
		set.Files = append(set.Files, artifactFile{
			Name: name, URL: mmproj.GetUrl(), SHA256: mmproj.GetSha256(), SizeBytes: int64(mmproj.GetSizeBytes()), Mmproj: true,
		})
	}
	seen := map[string]bool{}
	for _, f := range set.Files {
		if seen[f.Name] {
			return artifactSet{}, fmt.Errorf("models: %s: two files named %s", id, f.Name)
		}
		seen[f.Name] = true
	}
	return set, nil
}

// urlBasename is the upstream file name of an artifact URL: the last path
// segment, percent-decoded, and safe to join under the model directory.
// The catalog arrives over the network, so a segment that could escape
// the directory or hide as a dotfile is refused rather than cleaned.
func urlBasename(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("artifact url %q: %w", raw, err)
	}
	name := path.Base(u.Path)
	if dec, err := url.PathUnescape(name); err == nil {
		name = dec
	}
	switch {
	case name == "" || name == "." || name == "/" || name == "..":
		return "", fmt.Errorf("artifact url %q has no file name", raw)
	case name[0] == '.', len(name) > 255,
		strings.ContainsAny(name, "/\\\x00"),
		strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return "", fmt.Errorf("artifact url %q: unsafe file name %q", raw, name)
	}
	return name, nil
}

// partEntry is one file of a multi-file cache entry as persisted in
// models.json. State is StateReady once the file has been verified and
// renamed into place; empty while it is still to come.
type partEntry struct {
	Name      string `json:"name"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
	State     string `json:"state,omitempty"`
}

// multi reports whether the entry uses the <Dir>/<id>/ layout.
func (e *cacheEntry) multi() bool { return len(e.Parts) > 0 || e.Mmproj != nil }

// files lists the entry's files in download order: the shards, then the
// mmproj sidecar. A single-file entry yields nil; see Manager.entryFiles.
func (e *cacheEntry) files() []*partEntry {
	out := make([]*partEntry, 0, len(e.Parts)+1)
	for i := range e.Parts {
		out = append(out, &e.Parts[i])
	}
	if e.Mmproj != nil {
		out = append(out, e.Mmproj)
	}
	return out
}

// partsDone counts the files verified so far.
func (e *cacheEntry) partsDone() int {
	n := 0
	for _, f := range e.files() {
		if f.State == StateReady {
			n++
		}
	}
	return n
}

// fileRef is a file of a cache entry with its absolute path and the hash
// it must verify against.
type fileRef struct {
	Path   string
	SHA256 string
	Mmproj bool
}

// entryFiles lists every file that belongs to an entry, resolving the
// layout: <Dir>/<id>.gguf for a single-file entry, the subdirectory's
// parts and mmproj for a multi-file one. Callers hold m.mu or own e.
func (m *Manager) entryFiles(e *cacheEntry) []fileRef {
	if !e.multi() {
		return []fileRef{{Path: filepath.Join(m.Dir, e.ID+".gguf"), SHA256: e.SHA256}}
	}
	dir := filepath.Join(m.Dir, e.ID)
	files := e.files()
	out := make([]fileRef, 0, len(files))
	for _, f := range files {
		out = append(out, fileRef{Path: filepath.Join(dir, f.Name), SHA256: f.SHA256, Mmproj: f == e.Mmproj})
	}
	return out
}

// artifact is the Artifact a ready entry hands the runtime.
func (m *Manager) artifact(e *cacheEntry) Artifact {
	a := Artifact{SizeBytes: e.SizeBytes}
	for _, f := range m.entryFiles(e) {
		switch {
		case f.Mmproj:
			a.MmprojPath = f.Path
		case a.Path == "":
			a.Path = f.Path // the file, or part 1: llama-server finds the rest
		}
	}
	return a
}
