package llamacpp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestFetcherEnsureDownloadsAndVerifies(t *testing.T) {
	binary := []byte("#!/bin/sh\necho fake llama-server\n")
	sum := sha256.Sum256(binary)

	mux := http.NewServeMux()
	mux.HandleFunc("/llama-server", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(binary)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	man := ArtifactManifest{
		BuildID: "llamacpp-test-1",
		Builds: []ArtifactBuild{{
			OS: runtime.GOOS, Arch: runtime.GOARCH, Accel: "metal",
			URL: srv.URL + "/llama-server", SHA256: hex.EncodeToString(sum[:]),
		}},
	}
	manJSON, _ := json.Marshal(man)
	mux.HandleFunc("/manifest.json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(manJSON)
	})

	dir := t.TempDir()
	f := &Fetcher{ManifestURL: srv.URL + "/manifest.json", CacheDir: dir}
	path, buildID, err := f.Ensure(context.Background(), "metal")
	if err != nil {
		t.Fatal(err)
	}
	if buildID != "llamacpp-test-1" {
		t.Errorf("buildID = %q", buildID)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(binary) {
		t.Fatalf("downloaded content mismatch: %v", err)
	}
	// Second call: cached, no re-download needed even if server dies.
	srv.Close()
	// manifest fetch will fail though — so this exercises the cache only if
	// manifest is reachable; use a fresh fetcher against a stub manifest file.
	path2, _, err := (&Fetcher{ManifestURL: "http://127.0.0.1:1/manifest.json", CacheDir: dir, BinaryPath: path}).Ensure(context.Background(), "metal")
	if err != nil || path2 != path {
		t.Fatalf("BinaryPath override: %v", err)
	}
}

func TestFetcherRejectsSHAMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "manifest.json") {
			man := ArtifactManifest{BuildID: "b", Builds: []ArtifactBuild{{
				OS: runtime.GOOS, Arch: runtime.GOARCH, Accel: "cpu",
				URL: "http://" + r.Host + "/bin", SHA256: strings.Repeat("0", 64),
			}}}
			_ = json.NewEncoder(w).Encode(man)
			return
		}
		_, _ = w.Write([]byte("evil bytes"))
	}))
	defer srv.Close()

	f := &Fetcher{ManifestURL: srv.URL + "/manifest.json", CacheDir: t.TempDir()}
	_, _, err := f.Ensure(context.Background(), "cpu")
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("want sha mismatch error, got %v", err)
	}
}

func TestManifestPickCPUFallback(t *testing.T) {
	m := &ArtifactManifest{Builds: []ArtifactBuild{
		{OS: "linux", Arch: "amd64", Accel: "cuda12"},
		{OS: "linux", Arch: "amd64", Accel: "cpu-avx2"},
	}}
	b, err := m.pick("linux", "amd64", "rocm")
	if err != nil || b.Accel != "cpu-avx2" {
		t.Fatalf("fallback pick = %+v, %v", b, err)
	}
	if _, err := m.pick("darwin", "arm64", "metal"); err == nil {
		t.Fatal("expected no-build error")
	}
}

func TestFetcherRequiresConfig(t *testing.T) {
	f := &Fetcher{CacheDir: t.TempDir()}
	_, _, err := f.Ensure(context.Background(), "metal")
	if err == nil {
		t.Fatal("expected error when neither manifest nor binary configured")
	}
}

func TestFetcherBinaryPathMissing(t *testing.T) {
	f := &Fetcher{BinaryPath: filepath.Join(t.TempDir(), "nope")}
	if _, _, err := f.Ensure(context.Background(), "metal"); err == nil {
		t.Fatal("expected error for missing binary path")
	}
}
