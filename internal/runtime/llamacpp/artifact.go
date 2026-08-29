// Package llamacpp adapts a supervised llama-server subprocess to the
// runtime.Runtime interface (SPEC §A1.3: subprocess, never cgo).
package llamacpp

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ArtifactManifest mirrors teraflock/runtimes manifests/schema.json: one
// manifest per runtime_build_id, published alongside the tarballs. The
// build id participates in the fingerprint trust tuple
// (model_sha, quant, runtime_build_id) — SPEC §2.2. Fields the daemon does
// not act on (upstream_tag, created_at, …) are ignored rather than
// modelled, so a manifest format addition never requires a daemon release.
type ArtifactManifest struct {
	RuntimeBuildID string     `json:"runtime_build_id"`
	Artifacts      []Artifact `json:"artifacts"`
}

type Artifact struct {
	OS     string `json:"os"`    // darwin|linux|windows
	Arch   string `json:"arch"`  // arm64|amd64
	Accel  string `json:"accel"` // metal|cuda12|rocm|vulkan|cpu-avx2
	URL    string `json:"url"`
	SHA256 string `json:"sha256"` // of the tarball
	Size   int64  `json:"size_bytes"`
	// TODO(cosign): verify cosign_signature_url once the signing identity
	// exists (runtimes README step 3). SHA-256 pinning is enforced today.
}

// Fetcher downloads, SHA-verifies and unpacks the pinned llama-server build.
type Fetcher struct {
	// ManifestURL points at the ArtifactManifest JSON. Required unless
	// BinaryPath overrides fetching entirely.
	ManifestURL string
	// BinaryPath, when set, skips download and uses an existing binary
	// (config runtime.llama_server_path).
	BinaryPath string
	CacheDir   string // e.g. <data_dir>/runtimes
	HTTPClient *http.Client
}

// Preflight checks that a runtime binary is *resolvable* for this
// (GOOS, GOARCH, accel) without actually downloading the tarball. Used by
// `tera up` to refuse installing a service unit that would crash-loop on
// first start when the pinned catalog has no build for this machine.
// Returns nil when either f.BinaryPath is present on disk or f.ManifestURL
// advertises a matching artifact (cpu fallback included, same rule as
// Ensure).
func (f *Fetcher) Preflight(ctx context.Context, accel string) error {
	if f.BinaryPath != "" {
		if _, err := os.Stat(f.BinaryPath); err != nil {
			return fmt.Errorf("llamacpp: configured llama_server_path: %w", err)
		}
		return nil
	}
	if f.ManifestURL == "" {
		return fmt.Errorf("llamacpp: no runtime binary available: set runtime.llama_server_path to an existing llama-server binary or runtime.artifact_manifest_url to a pinned build manifest")
	}
	man, err := f.fetchManifest(ctx)
	if err != nil {
		return err
	}
	_, err = man.pick(runtime.GOOS, runtime.GOARCH, accel)
	return err
}

// Ensure returns the path to a verified llama-server binary and its build id.
func (f *Fetcher) Ensure(ctx context.Context, accel string) (path, buildID string, err error) {
	if f.BinaryPath != "" {
		if _, err := os.Stat(f.BinaryPath); err != nil {
			return "", "", fmt.Errorf("llamacpp: configured llama_server_path: %w", err)
		}
		return f.BinaryPath, "local-binary", nil
	}
	if f.ManifestURL == "" {
		return "", "", fmt.Errorf("llamacpp: no runtime binary available: set runtime.llama_server_path to an existing llama-server binary or runtime.artifact_manifest_url to a pinned build manifest")
	}

	man, err := f.fetchManifest(ctx)
	if err != nil {
		return "", "", err
	}
	art, err := man.pick(runtime.GOOS, runtime.GOARCH, accel)
	if err != nil {
		return "", "", err
	}

	dir := filepath.Join(f.CacheDir, man.RuntimeBuildID)
	bin := filepath.Join(dir, binaryName())
	// The stored SHA is of the tarball, not the binary, so reuse is gated
	// on a stamp written after a verified extract.
	if sha, err := os.ReadFile(filepath.Join(dir, ".tarball.sha256")); err == nil &&
		strings.TrimSpace(string(sha)) == art.SHA256 {
		if _, err := os.Stat(bin); err == nil {
			return bin, man.RuntimeBuildID, nil
		}
	}
	if err := f.fetchAndUnpack(ctx, art, dir); err != nil {
		return "", "", err
	}
	return bin, man.RuntimeBuildID, nil
}

func (f *Fetcher) client() *http.Client {
	if f.HTTPClient != nil {
		return f.HTTPClient
	}
	return &http.Client{Timeout: 10 * time.Minute}
}

func (f *Fetcher) fetchManifest(ctx context.Context) (*ArtifactManifest, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.ManifestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("llamacpp: manifest request: %w", err)
	}
	resp, err := f.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("llamacpp: fetch manifest: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("llamacpp: fetch manifest: unexpected status %s", resp.Status)
	}
	var m ArtifactManifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&m); err != nil {
		return nil, fmt.Errorf("llamacpp: decode manifest: %w", err)
	}
	if m.RuntimeBuildID == "" || len(m.Artifacts) == 0 {
		return nil, fmt.Errorf("llamacpp: manifest at %s has no runtime_build_id/artifacts (wrong URL or format?)", f.ManifestURL)
	}
	return &m, nil
}

func (m *ArtifactManifest) pick(goos, goarch, accel string) (Artifact, error) {
	var cpuFallback *Artifact
	for i, a := range m.Artifacts {
		if a.OS != goos || a.Arch != goarch {
			continue
		}
		if a.Accel == accel {
			return a, nil
		}
		if a.Accel == "cpu" || a.Accel == "cpu-avx2" {
			cpuFallback = &m.Artifacts[i]
		}
	}
	if cpuFallback != nil {
		return *cpuFallback, nil
	}
	return Artifact{}, fmt.Errorf("llamacpp: manifest %s has no build for %s/%s accel=%s", m.RuntimeBuildID, goos, goarch, accel)
}

// fetchAndUnpack downloads the tarball, verifies its SHA-256, and extracts
// llama-server (plus LICENSE/BUILDINFO) into dir. The tarball layout is
// llama-server-<tag>-<os>-<arch>-<accel>/{llama-server,LICENSE.llama.cpp,BUILDINFO};
// entries are extracted by basename to fixed paths, so hostile archive
// paths cannot escape dir.
func (f *Fetcher) fetchAndUnpack(ctx context.Context, a Artifact, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("llamacpp: mkdir: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return fmt.Errorf("llamacpp: artifact request: %w", err)
	}
	resp, err := f.client().Do(req)
	if err != nil {
		return fmt.Errorf("llamacpp: download artifact: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("llamacpp: download artifact: unexpected status %s", resp.Status)
	}

	// Stage the whole tarball first: nothing is unpacked before the hash
	// over the complete file matches the manifest.
	tarball := filepath.Join(dir, ".download.tar.gz")
	out, err := os.OpenFile(tarball, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("llamacpp: create temp: %w", err)
	}
	h := sha256.New()
	_, cpErr := io.Copy(io.MultiWriter(out, h), resp.Body)
	closeErr := out.Close()
	if cpErr != nil {
		_ = os.Remove(tarball)
		return fmt.Errorf("llamacpp: write artifact: %w", cpErr)
	}
	if closeErr != nil {
		return fmt.Errorf("llamacpp: close artifact: %w", closeErr)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != a.SHA256 {
		_ = os.Remove(tarball)
		return fmt.Errorf("llamacpp: artifact sha256 mismatch: got %s want %s (refusing to run unverified runtime)", got, a.SHA256)
	}
	if err := extractRuntime(tarball, dir); err != nil {
		_ = os.Remove(tarball)
		return err
	}
	_ = os.Remove(tarball)
	// Stamp last: its presence means "verified tarball, complete extract".
	if err := os.WriteFile(filepath.Join(dir, ".tarball.sha256"), []byte(a.SHA256+"\n"), 0o644); err != nil {
		return fmt.Errorf("llamacpp: write verify stamp: %w", err)
	}
	return nil
}

// extractRuntime unpacks the known members of a runtimes tarball into dir.
func extractRuntime(tarball, dir string) error {
	f, err := os.Open(tarball)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("llamacpp: artifact is not gzip: %w", err)
	}
	defer gz.Close()

	found := false
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("llamacpp: read tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		var dest string
		var mode os.FileMode
		switch filepath.Base(hdr.Name) {
		case binaryName():
			dest, mode, found = filepath.Join(dir, binaryName()), 0o755, true
		case "LICENSE.llama.cpp", "BUILDINFO":
			dest, mode = filepath.Join(dir, filepath.Base(hdr.Name)), 0o644
		default:
			continue
		}
		w, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
		if err != nil {
			return fmt.Errorf("llamacpp: extract %s: %w", filepath.Base(hdr.Name), err)
		}
		_, cpErr := io.Copy(w, tr) //nolint:gosec // size bounded by verified tarball
		if closeErr := w.Close(); cpErr != nil || closeErr != nil {
			return fmt.Errorf("llamacpp: extract %s: copy=%v close=%v", filepath.Base(hdr.Name), cpErr, closeErr)
		}
	}
	if !found {
		return fmt.Errorf("llamacpp: tarball contains no %s", binaryName())
	}
	return nil
}

func binaryName() string {
	if runtime.GOOS == "windows" {
		return "llama-server.exe"
	}
	return "llama-server"
}
