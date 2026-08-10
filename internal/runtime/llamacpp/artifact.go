// Package llamacpp adapts a supervised llama-server subprocess to the
// runtime.Runtime interface (SPEC §A1.3: subprocess, never cgo).
package llamacpp

import (
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
	"time"
)

// ArtifactManifest describes pinned llama-server builds per (os, arch,
// accel), published by the private hivegrid/runtimes CI to the artifact CDN.
// The build id participates in the fingerprint trust tuple
// (model_sha, quant, runtime_build_id) — SPEC §2.2.
type ArtifactManifest struct {
	// BuildID like "llamacpp-b4458-hive1".
	BuildID string          `json:"build_id"`
	Builds  []ArtifactBuild `json:"builds"`
}

type ArtifactBuild struct {
	OS     string `json:"os"`    // darwin|linux|windows
	Arch   string `json:"arch"`  // arm64|amd64
	Accel  string `json:"accel"` // metal|cuda12|rocm|vulkan|cpu-avx2|cpu
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size_bytes"`
}

// Fetcher downloads and SHA-verifies the llama-server binary.
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
	build, err := man.pick(runtime.GOOS, runtime.GOARCH, accel)
	if err != nil {
		return "", "", err
	}

	dest := filepath.Join(f.CacheDir, man.BuildID, binaryName())
	if ok, _ := verifyFile(dest, build.SHA256); ok {
		return dest, man.BuildID, nil
	}
	if err := f.download(ctx, build, dest); err != nil {
		return "", "", err
	}
	return dest, man.BuildID, nil
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
	return &m, nil
}

func (m *ArtifactManifest) pick(goos, goarch, accel string) (ArtifactBuild, error) {
	var cpuFallback *ArtifactBuild
	for i, b := range m.Builds {
		if b.OS != goos || b.Arch != goarch {
			continue
		}
		if b.Accel == accel {
			return b, nil
		}
		if b.Accel == "cpu" || b.Accel == "cpu-avx2" {
			cpuFallback = &m.Builds[i]
		}
	}
	if cpuFallback != nil {
		return *cpuFallback, nil
	}
	return ArtifactBuild{}, fmt.Errorf("llamacpp: manifest %s has no build for %s/%s accel=%s", m.BuildID, goos, goarch, accel)
}

func (f *Fetcher) download(ctx context.Context, b ArtifactBuild, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("llamacpp: mkdir: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.URL, nil)
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

	tmp := dest + ".partial"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755) //nolint:gosec // executable
	if err != nil {
		return fmt.Errorf("llamacpp: create temp: %w", err)
	}
	h := sha256.New()
	_, cpErr := io.Copy(io.MultiWriter(out, h), resp.Body)
	closeErr := out.Close()
	if cpErr != nil {
		return fmt.Errorf("llamacpp: write artifact: %w", cpErr)
	}
	if closeErr != nil {
		return fmt.Errorf("llamacpp: close artifact: %w", closeErr)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != b.SHA256 {
		_ = os.Remove(tmp)
		return fmt.Errorf("llamacpp: artifact sha256 mismatch: got %s want %s (refusing to run unverified runtime)", got, b.SHA256)
	}
	if err := os.Rename(tmp, dest); err != nil {
		return fmt.Errorf("llamacpp: finalize artifact: %w", err)
	}
	return nil
}

func verifyFile(path, wantSHA string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == wantSHA, nil
}

func binaryName() string {
	if runtime.GOOS == "windows" {
		return "llama-server.exe"
	}
	return "llama-server"
}
