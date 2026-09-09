package llamacpp

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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

// makeTarball builds a runtimes-shaped tarball: a top-level versioned
// directory holding llama-server, LICENSE and BUILDINFO.
func makeTarball(t *testing.T, binary []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	dir := "llama-server-b9999-test-test-test/"
	for _, f := range []struct {
		name string
		body []byte
		mode int64
	}{
		{dir + binaryName(), binary, 0o755},
		{dir + "LICENSE.llama.cpp", []byte("MIT"), 0o644},
		{dir + "BUILDINFO", []byte("tag=b9999\n"), 0o644},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(f.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func serveBuild(t *testing.T, tarball []byte) (*httptest.Server, string) {
	t.Helper()
	sum := sha256.Sum256(tarball)
	mux := http.NewServeMux()
	mux.HandleFunc("/llama-server.tar.gz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarball)
	})
	srv := httptest.NewServer(mux)
	man := ArtifactManifest{
		RuntimeBuildID: "llamacpp-b9999-1",
		Artifacts: []Artifact{{
			OS: runtime.GOOS, Arch: runtime.GOARCH, Accel: "metal",
			URL: srv.URL + "/llama-server.tar.gz", SHA256: hex.EncodeToString(sum[:]),
			Size: int64(len(tarball)),
		}},
	}
	manJSON, _ := json.Marshal(man)
	mux.HandleFunc("/manifest.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(manJSON)
	})
	return srv, srv.URL + "/manifest.json"
}

func TestFetcherDownloadsVerifiesAndUnpacks(t *testing.T) {
	binary := []byte("#!/bin/sh\necho fake llama-server\n")
	srv, manifestURL := serveBuild(t, makeTarball(t, binary))
	defer srv.Close()

	dir := t.TempDir()
	f := &Fetcher{ManifestURL: manifestURL, CacheDir: dir}
	path, buildID, err := f.Ensure(context.Background(), "metal")
	if err != nil {
		t.Fatal(err)
	}
	if buildID != "llamacpp-b9999-1" {
		t.Errorf("buildID = %q", buildID)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(binary) {
		t.Fatalf("extracted binary mismatch: %v", err)
	}
	if fi, _ := os.Stat(path); runtime.GOOS != "windows" && fi.Mode().Perm()&0o111 == 0 {
		t.Error("extracted binary is not executable")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "BUILDINFO")); err != nil {
		t.Error("BUILDINFO not extracted alongside the binary")
	}

	// Second Ensure must reuse the verified extract (no re-download): kill
	// the artifact route, keep only the manifest reachable.
	binPath := path
	mux := http.NewServeMux()
	man := struct {
		ID   string     `json:"runtime_build_id"`
		Arts []Artifact `json:"artifacts"`
	}{ID: "llamacpp-b9999-1", Arts: []Artifact{{
		OS: runtime.GOOS, Arch: runtime.GOARCH, Accel: "metal",
		URL: "http://127.0.0.1:1/gone.tar.gz", SHA256: readStamp(t, filepath.Dir(path)),
	}}}
	manJSON, _ := json.Marshal(man)
	mux.HandleFunc("/manifest.json", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(manJSON) })
	srv2 := httptest.NewServer(mux)
	defer srv2.Close()
	path2, _, err := (&Fetcher{ManifestURL: srv2.URL + "/manifest.json", CacheDir: dir}).Ensure(context.Background(), "metal")
	if err != nil || path2 != binPath {
		t.Fatalf("cached reuse failed: %q, %v", path2, err)
	}
}

func readStamp(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, ".tarball.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func TestFetcherRejectsSHAMismatch(t *testing.T) {
	tarball := makeTarball(t, []byte("evil"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "manifest.json") {
			man := ArtifactManifest{RuntimeBuildID: "llamacpp-b9999-1", Artifacts: []Artifact{{
				OS: runtime.GOOS, Arch: runtime.GOARCH, Accel: "cpu-avx2",
				URL: "http://" + r.Host + "/bin.tar.gz", SHA256: strings.Repeat("0", 64),
			}}}
			_ = json.NewEncoder(w).Encode(man)
			return
		}
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	dir := t.TempDir()
	f := &Fetcher{ManifestURL: srv.URL + "/manifest.json", CacheDir: dir}
	_, _, err := f.Ensure(context.Background(), "cpu-avx2")
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("want sha mismatch error, got %v", err)
	}
	// Nothing runnable may be left behind after a failed verify.
	if _, err := os.Stat(filepath.Join(dir, "llamacpp-b9999-1", binaryName())); err == nil {
		t.Fatal("unverified binary was extracted")
	}
}

func TestFetcherRejectsTarballWithoutBinary(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "nothing/README", Mode: 0o644, Size: 2})
	_, _ = tw.Write([]byte("hi"))
	_ = tw.Close()
	_ = gz.Close()
	srv, manifestURL := serveBuild(t, buf.Bytes())
	defer srv.Close()

	_, _, err := (&Fetcher{ManifestURL: manifestURL, CacheDir: t.TempDir()}).Ensure(context.Background(), "metal")
	if err == nil || !strings.Contains(err.Error(), "no "+binaryName()) {
		t.Fatalf("want missing-binary error, got %v", err)
	}
}

func TestFetcherRejectsEmptyManifest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// The pre-fix flockd shape: proof that a wrong-format manifest is a
		// loud error, not an empty struct that fails later.
		_, _ = w.Write([]byte(`{"build_id":"x","builds":[{"os":"darwin"}]}`))
	}))
	defer srv.Close()
	_, _, err := (&Fetcher{ManifestURL: srv.URL, CacheDir: t.TempDir()}).Ensure(context.Background(), "metal")
	if err == nil || !strings.Contains(err.Error(), "no runtime_build_id") {
		t.Fatalf("want format error, got %v", err)
	}
}

func TestManifestPickCPUFallback(t *testing.T) {
	m := &ArtifactManifest{RuntimeBuildID: "llamacpp-b9999-1", Artifacts: []Artifact{
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

// The flockd#21 chain: cuda12 > rocm > vulkan > cpu. The first published
// lane wins; unpublished GPU lanes are skipped, not treated as CPU.
func TestManifestPickWalksAccelChain(t *testing.T) {
	m := &ArtifactManifest{RuntimeBuildID: "llamacpp-b9999-1", Artifacts: []Artifact{
		{OS: "linux", Arch: "amd64", Accel: "cpu-avx2"},
		{OS: "linux", Arch: "amd64", Accel: "vulkan"},
		{OS: "linux", Arch: "amd64", Accel: "cuda12"},
		{OS: "linux", Arch: "arm64", Accel: "cpu"},
	}}
	cases := []struct {
		chain []string
		want  string
	}{
		{[]string{"cuda12", "vulkan", "cpu-avx2"}, "cuda12"},
		{[]string{"rocm", "vulkan", "cpu-avx2"}, "vulkan"}, // AMD box, no rocm lane published
		{[]string{"rocm", "cpu-avx2"}, "cpu-avx2"},         // AMD box without a Vulkan driver
		{[]string{"vulkan", "cpu-avx2"}, "vulkan"},         // Intel Arc
		{[]string{"cpu-avx2"}, "cpu-avx2"},
		{nil, "cpu-avx2"},
		{[]string{"cpu"}, "cpu-avx2"}, // any CPU build satisfies a CPU request
	}
	for _, c := range cases {
		b, err := m.pick("linux", "amd64", c.chain...)
		if err != nil || b.Accel != c.want {
			t.Errorf("pick(%v) = %s, %v; want %s", c.chain, b.Accel, err, c.want)
		}
	}
	if _, err := m.pick("linux", "arm64", "vulkan", "cpu-avx2"); err != nil {
		t.Errorf("arm64 should fall to its cpu build, got %v", err)
	}
	if _, err := m.pick("windows", "amd64", "cuda12", "cpu-avx2"); err == nil || !strings.Contains(err.Error(), "accel=cuda12>cpu-avx2") {
		t.Errorf("want no-build error naming the chain, got %v", err)
	}
}

func TestPickReason(t *testing.T) {
	cases := []struct {
		chain  []string
		chosen string
		want   string
	}{
		{[]string{"cuda12", "vulkan", "cpu-avx2"}, "cuda12", "preferred"},
		{[]string{"rocm", "vulkan", "cpu-avx2"}, "vulkan", "no rocm artifact for linux/amd64"},
		{[]string{"rocm", "vulkan", "cpu-avx2"}, "cpu-avx2", "no rocm/vulkan artifact for linux/amd64"},
		{[]string{"cpu-avx2"}, "cpu-avx2", "preferred"},
		{[]string{"cpu-avx2"}, "cpu", "preferred"},
	}
	for _, c := range cases {
		if got := pickReason("linux", "amd64", c.chain, c.chosen); got != c.want {
			t.Errorf("pickReason(%v, %s) = %q, want %q", c.chain, c.chosen, got, c.want)
		}
	}
}

func TestEnsureSelectionReportsChosenAccel(t *testing.T) {
	tarball := makeTarball(t, []byte("#!/bin/sh\necho vulkan\n"))
	srv, _ := serveBuild(t, tarball)
	defer srv.Close()
	sum := sha256.Sum256(tarball)
	man := ArtifactManifest{RuntimeBuildID: "llamacpp-b9999-1", Artifacts: []Artifact{
		{OS: runtime.GOOS, Arch: runtime.GOARCH, Accel: "vulkan", URL: srv.URL + "/llama-server.tar.gz", SHA256: hex.EncodeToString(sum[:])},
	}}
	mj, _ := json.Marshal(man)
	msrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(mj) }))
	defer msrv.Close()

	f := &Fetcher{ManifestURL: msrv.URL, CacheDir: t.TempDir()}
	sel, err := f.EnsureSelection(context.Background(), "rocm", "vulkan", "cpu-avx2")
	if err != nil {
		t.Fatal(err)
	}
	if sel.Accel != "vulkan" || sel.BuildID != "llamacpp-b9999-1" || !strings.HasPrefix(sel.Reason, "no rocm artifact for") {
		t.Errorf("selection = %+v", sel)
	}
	if _, err := os.Stat(sel.Path); err != nil {
		t.Errorf("binary not extracted: %v", err)
	}
	// Preflight walks the same chain.
	if err := f.Preflight(context.Background(), "rocm", "vulkan", "cpu-avx2"); err != nil {
		t.Errorf("preflight: %v", err)
	}
	if err := f.Preflight(context.Background(), "rocm", "cpu-avx2"); err == nil {
		t.Error("preflight without a matching lane and no cpu build should fail")
	}

	bin := filepath.Join(t.TempDir(), "llama-server")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sel, err = (&Fetcher{BinaryPath: bin}).EnsureSelection(context.Background(), "cuda12")
	if err != nil || sel.Accel != "" || sel.BuildID != "local-binary" {
		t.Errorf("local binary selection = %+v, %v", sel, err)
	}
}

func TestFetcherRequiresConfig(t *testing.T) {
	f := &Fetcher{CacheDir: t.TempDir()}
	if _, _, err := f.Ensure(context.Background(), "metal"); err == nil {
		t.Fatal("expected error when neither manifest nor binary configured")
	}
}

func TestFetcherBinaryPathMissing(t *testing.T) {
	f := &Fetcher{BinaryPath: filepath.Join(t.TempDir(), "nope")}
	if _, _, err := f.Ensure(context.Background(), "metal"); err == nil {
		t.Fatal("expected error for missing binary path")
	}
}

// Preflight is what `tera up` calls before installing a service unit —
// it must reject platforms the catalog has no build for, so we don't
// leave a crash-looping daemon behind. This is the exact bug that
// prompted its introduction: a linux/amd64 CUDA box against a catalog
// with only darwin/arm64 artifacts.
//
// Use plan9 as the artifact OS so no real CI runner accidentally
// satisfies the manifest — the assertion is "when no build matches, we
// error", and that has to hold on every runner.
func TestPreflightRejectsPlatformWithNoBuild(t *testing.T) {
	man := ArtifactManifest{RuntimeBuildID: "llamacpp-b9999-1", Artifacts: []Artifact{
		{OS: "plan9", Arch: "amd64", Accel: "metal", URL: "http://x", SHA256: strings.Repeat("0", 64)},
	}}
	mj, _ := json.Marshal(man)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(mj) }))
	defer srv.Close()

	err := (&Fetcher{ManifestURL: srv.URL}).Preflight(context.Background(), "cuda12")
	if err == nil || !strings.Contains(err.Error(), "no build for") {
		t.Fatalf("want no-build error, got %v", err)
	}
}

func TestPreflightAcceptsCPUFallback(t *testing.T) {
	man := ArtifactManifest{RuntimeBuildID: "llamacpp-b9999-1", Artifacts: []Artifact{
		{OS: runtime.GOOS, Arch: runtime.GOARCH, Accel: "cpu-avx2", URL: "http://x", SHA256: strings.Repeat("0", 64)},
	}}
	mj, _ := json.Marshal(man)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(mj) }))
	defer srv.Close()

	// Asking for a GPU accel that the manifest doesn't advertise must still
	// succeed via the cpu-avx2 fallback — same rule as Ensure.
	if err := (&Fetcher{ManifestURL: srv.URL}).Preflight(context.Background(), "cuda12"); err != nil {
		t.Fatalf("cpu fallback should satisfy preflight, got %v", err)
	}
}

func TestPreflightAcceptsExistingLocalBinary(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "llama-server")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// BinaryPath skips the manifest entirely (no URL configured is fine).
	if err := (&Fetcher{BinaryPath: bin}).Preflight(context.Background(), "cuda12"); err != nil {
		t.Fatalf("existing binary should satisfy preflight, got %v", err)
	}
}
