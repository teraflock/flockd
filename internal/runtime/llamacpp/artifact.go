// Package llamacpp adapts a supervised llama-server subprocess to the
// runtime.Runtime interface (SPEC §A1.3: subprocess, never cgo).
package llamacpp

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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
	"runtime"
	"strings"
	"sync"
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
	// CosignSignatureURL is the detached `cosign sign-blob` signature over
	// the tarball: a bare base64 DER ECDSA signature over its SHA-256
	// (.sig) or a Sigstore bundle carrying the same (.sigstore.json); see
	// trust.go. Verified against the pinned key before anything is
	// unpacked; optional until every published manifest carries one
	// (runtime.require_signature).
	CosignSignatureURL string `json:"cosign_signature_url"`
	// CosignCertificateURL is modelled because the runtimes schema defines
	// it, but unused: key-mode signatures have no certificate, and a
	// certificate served by the artifact host proves nothing on its own.
	// Only a keyless (Fulcio) custody decision would make it load-bearing;
	// see trust.go.
	CosignCertificateURL string `json:"cosign_certificate_url"`
}

// Trust stamps (.tarball.verified): the policy under which an extracted
// build was trusted. A cached extract is reused only when its stamp
// satisfies the current policy, so a build unpacked under sha256-only
// is re-fetched and signature-verified once require_signature flips.
const (
	trustSHA256Only = "sha256-only"
	trustCosign     = "cosign"

	stampSHA256   = ".tarball.sha256"
	stampVerified = ".tarball.verified"
)

// Fetcher downloads, SHA-verifies, signature-verifies and unpacks the
// pinned llama-server build.
type Fetcher struct {
	// ManifestURL points at the ArtifactManifest JSON. Required unless
	// BinaryPath overrides fetching entirely.
	ManifestURL string
	// BinaryPath, when set, skips download and uses an existing binary
	// (config runtime.llama_server_path).
	BinaryPath string
	CacheDir   string // e.g. <data_dir>/runtimes
	HTTPClient *http.Client

	// SigningKeyPEM is the resolved public key (PKIX PEM) that artifact and
	// manifest signatures are verified against (config
	// runtime.artifact_signing_key). nil falls back to the daemon's
	// embedded pin (trust.go); when that is empty too there is no verifier
	// and signatures cannot be checked.
	SigningKeyPEM []byte
	// RequireSignature refuses manifests and artifacts that advertise no
	// cosign signature, refuses cached extracts trusted by sha256 only, and
	// refuses to run at all without a pinned key (config
	// runtime.require_signature).
	RequireSignature bool
	// Log receives the one-time trust warnings and the "runtime signature
	// verified" line; nil uses slog.Default().
	Log *slog.Logger

	warnUnsignedOnce   sync.Once
	warnNoVerifierOnce sync.Once
}

// Preflight checks that a runtime binary is *resolvable* for this
// (GOOS, GOARCH) and accelerator chain without actually downloading the
// tarball. Used by `tera up` to refuse installing a service unit that
// would crash-loop on first start when the pinned catalog has no build
// for this machine. Returns nil when either f.BinaryPath is present on
// disk or f.ManifestURL advertises a matching artifact (cpu fallback
// included, same rule as Ensure) that satisfies the signature policy.
func (f *Fetcher) Preflight(ctx context.Context, accels ...string) error {
	if f.BinaryPath != "" {
		if _, err := os.Stat(f.BinaryPath); err != nil {
			return fmt.Errorf("llamacpp: configured llama_server_path: %w", err)
		}
		return nil
	}
	if f.ManifestURL == "" {
		return errNoRuntimeConfigured
	}
	man, err := f.fetchManifest(ctx)
	if err != nil {
		return err
	}
	art, err := man.pick(runtime.GOOS, runtime.GOARCH, accels...)
	if err != nil {
		return err
	}
	return f.checkAdvertisedSignature(art)
}

var errNoRuntimeConfigured = fmt.Errorf("llamacpp: no runtime binary available: set runtime.llama_server_path to an existing llama-server binary or runtime.artifact_manifest_url to a pinned build manifest")

var errNoSigningKey = errors.New("llamacpp: runtime.require_signature is true but no runtime signing key is pinned: set runtime.artifact_signing_key to the publisher's public key (PEM), or use a daemon release that embeds the Teraflock key (SPEC §A3 Key custody, teraflock/docs#34)")

// Selection is the llama-server build Ensure resolved for this node.
type Selection struct {
	Path    string
	BuildID string
	// Accel is the backend of the build actually chosen — the first entry
	// of the requested chain the manifest publishes, or the CPU build.
	// Empty for a configured local binary (runtime.llama_server_path),
	// whose backend the daemon cannot know.
	Accel string
	// Reason says why that build was chosen, for the "runtime accel
	// selected" log line: "preferred", "no cuda12 artifact for linux/amd64",
	// "configured llama_server_path".
	Reason string
}

// Ensure returns the path to a verified llama-server binary and its build
// id for the accelerator chain (hardware.AccelPreference order: the first
// accel with a published build wins, then the CPU build).
func (f *Fetcher) Ensure(ctx context.Context, accels ...string) (path, buildID string, err error) {
	sel, err := f.EnsureSelection(ctx, accels...)
	return sel.Path, sel.BuildID, err
}

// EnsureSelection is Ensure plus which accel the build actually is.
func (f *Fetcher) EnsureSelection(ctx context.Context, accels ...string) (Selection, error) {
	if f.BinaryPath != "" {
		if _, err := os.Stat(f.BinaryPath); err != nil {
			return Selection{}, fmt.Errorf("llamacpp: configured llama_server_path: %w", err)
		}
		return Selection{Path: f.BinaryPath, BuildID: "local-binary", Reason: "configured llama_server_path"}, nil
	}
	if f.ManifestURL == "" {
		return Selection{}, errNoRuntimeConfigured
	}

	man, err := f.fetchManifest(ctx)
	if err != nil {
		return Selection{}, err
	}
	art, err := man.pick(runtime.GOOS, runtime.GOARCH, accels...)
	if err != nil {
		return Selection{}, err
	}
	if err := f.checkAdvertisedSignature(art); err != nil {
		return Selection{}, err
	}
	sel := Selection{
		BuildID: man.RuntimeBuildID,
		Accel:   art.Accel,
		Reason:  pickReason(runtime.GOOS, runtime.GOARCH, accels, art.Accel),
	}

	dir := filepath.Join(f.CacheDir, man.RuntimeBuildID)
	sel.Path = filepath.Join(dir, binaryName())
	// The stored SHA is of the tarball, not the binary, so reuse is gated
	// on a stamp written after a verified extract — and on that extract
	// having been trusted under a policy at least as strict as today's.
	if sha, err := os.ReadFile(filepath.Join(dir, stampSHA256)); err == nil &&
		strings.TrimSpace(string(sha)) == art.SHA256 {
		if _, err := os.Stat(sel.Path); err == nil {
			trust := readTrustStamp(dir)
			if !f.RequireSignature || trust == trustCosign {
				return sel, nil
			}
			f.log().Info("cached runtime was trusted by sha256 only; re-fetching to verify its signature",
				"runtime_build_id", man.RuntimeBuildID, "cached_trust", trust)
		}
	}
	if err := f.fetchAndUnpack(ctx, art, dir, man.RuntimeBuildID); err != nil {
		return Selection{}, err
	}
	return sel, nil
}

// readTrustStamp reports the policy an extract in dir was trusted under.
// Extracts from before the stamp existed count as sha256-only.
func readTrustStamp(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, stampVerified))
	if err != nil {
		return trustSHA256Only
	}
	if s := strings.TrimSpace(string(b)); s == trustCosign {
		return trustCosign
	}
	return trustSHA256Only
}

// checkAdvertisedSignature enforces the require_signature half of the
// policy that needs no network: a manifest that advertises no signature
// for the chosen build is refused outright. Shared by Preflight (so
// `tera up` reports it before installing a unit) and EnsureSelection.
func (f *Fetcher) checkAdvertisedSignature(a Artifact) error {
	if f.RequireSignature && a.CosignSignatureURL == "" {
		return fmt.Errorf("llamacpp: manifest advertises no cosign signature for %s/%s %s (%s); refusing (runtime.require_signature=true)", a.OS, a.Arch, a.Accel, a.URL)
	}
	return nil
}

// verifier resolves the pinned key: the config override first, then the
// embedded release pin. (nil, nil) means no key is pinned at all.
func (f *Fetcher) verifier() (*Verifier, error) {
	pemBytes := f.SigningKeyPEM
	if len(pemBytes) == 0 && embeddedRuntimeSigningKeyPEM != "" {
		pemBytes = []byte(embeddedRuntimeSigningKeyPEM)
	}
	if len(pemBytes) == 0 {
		return nil, nil
	}
	return ParseVerifier(pemBytes)
}

func (f *Fetcher) log() *slog.Logger {
	if f.Log != nil {
		return f.Log
	}
	return slog.Default()
}

func (f *Fetcher) client() *http.Client {
	if f.HTTPClient != nil {
		return f.HTTPClient
	}
	return &http.Client{Timeout: 10 * time.Minute}
}

// fetchManifest downloads and decodes the manifest. When a key is pinned
// it also fetches `<manifest-url>.sig` and verifies the manifest body
// against it, so a swapped manifest cannot point at an attacker's tarball
// no matter what that tarball's own signature says. 404 and 403 (S3's
// answer for a missing key) mean "unsigned manifest": fatal under
// RequireSignature, tolerated otherwise.
func (f *Fetcher) fetchManifest(ctx context.Context) (*ArtifactManifest, error) {
	v, err := f.verifier()
	if err != nil {
		return nil, err
	}
	if v == nil && f.RequireSignature {
		return nil, errNoSigningKey
	}

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
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("llamacpp: read manifest: %w", err)
	}

	if v != nil {
		sig, present, err := f.fetchSignature(ctx, f.ManifestURL+".sig", true)
		if err != nil {
			return nil, fmt.Errorf("llamacpp: manifest signature: %w", err)
		}
		switch {
		case present:
			digest := sha256.Sum256(body)
			if err := v.Verify(digest[:], sig); err != nil {
				return nil, fmt.Errorf("llamacpp: manifest signature invalid: %w (refusing to run unverified runtime)", err)
			}
			f.log().Debug("runtime manifest signature verified", "url", f.ManifestURL)
		case f.RequireSignature:
			return nil, fmt.Errorf("llamacpp: manifest at %s has no signature (%s.sig missing); refusing (runtime.require_signature=true)", f.ManifestURL, f.ManifestURL)
		default:
			f.log().Debug("runtime manifest is unsigned; trusting its artifact sha256 pins only", "url", f.ManifestURL)
		}
	}

	var m ArtifactManifest
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&m); err != nil {
		return nil, fmt.Errorf("llamacpp: decode manifest: %w", err)
	}
	if m.RuntimeBuildID == "" || len(m.Artifacts) == 0 {
		return nil, fmt.Errorf("llamacpp: manifest at %s has no runtime_build_id/artifacts (wrong URL or format?)", f.ManifestURL)
	}
	return &m, nil
}

// fetchSignature GETs a detached .sig (bounded read). With optional set,
// 404/403 report (nil, false, nil) instead of an error; any other
// non-200 status is an error either way.
func (f *Fetcher) fetchSignature(ctx context.Context, url string, optional bool) (sig []byte, present bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, fmt.Errorf("request %s: %w", url, err)
	}
	resp, err := f.client().Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if optional && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden) {
		return nil, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("fetch %s: unexpected status %s", url, resp.Status)
	}
	sig, err = io.ReadAll(io.LimitReader(resp.Body, maxSignatureBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", url, err)
	}
	if len(sig) > maxSignatureBytes {
		return nil, false, fmt.Errorf("%s is larger than %d bytes, not a signature", url, maxSignatureBytes)
	}
	return sig, true, nil
}

// verifyArtifactSignature applies the artifact half of the trust policy
// to a tarball whose sha256 already matched the manifest, and returns the
// trust stamp to record for the extract. digest is that sha256 — the
// exact bytes cosign sign-blob signed.
func (f *Fetcher) verifyArtifactSignature(ctx context.Context, a Artifact, digest []byte, buildID string) (string, error) {
	v, err := f.verifier()
	if err != nil {
		return "", err
	}
	if a.CosignSignatureURL == "" {
		if f.RequireSignature {
			return "", f.checkAdvertisedSignature(a)
		}
		f.warnUnsignedOnce.Do(func() {
			f.log().Warn("runtime artifact advertises no cosign signature; trusting sha256 only (set runtime.require_signature=true to refuse unsigned builds)",
				"runtime_build_id", buildID, "url", a.URL)
		})
		return trustSHA256Only, nil
	}
	if v == nil {
		if f.RequireSignature {
			return "", errNoSigningKey
		}
		f.warnNoVerifierOnce.Do(func() {
			f.log().Warn("runtime artifact is signed but no verifier is pinned (docs#34); trusting sha256 only",
				"runtime_build_id", buildID, "signature_url", a.CosignSignatureURL)
		})
		return trustSHA256Only, nil
	}
	sig, _, err := f.fetchSignature(ctx, a.CosignSignatureURL, false)
	if err != nil {
		return "", fmt.Errorf("llamacpp: artifact signature invalid: %w (refusing to run unverified runtime)", err)
	}
	if err := v.Verify(digest, sig); err != nil {
		return "", fmt.Errorf("llamacpp: artifact signature invalid: %w (refusing to run unverified runtime)", err)
	}
	f.log().Info("runtime signature verified", "runtime_build_id", buildID, "accel", a.Accel)
	return trustCosign, nil
}

// pick walks the accelerator chain in order and returns the first
// published build for this OS/arch; the CPU build (cpu-avx2 or cpu) is the
// implicit last entry, so a chain whose GPU lanes are all unpublished
// still resolves. Error only when not even a CPU build exists.
func (m *ArtifactManifest) pick(goos, goarch string, accels ...string) (Artifact, error) {
	var cpuFallback *Artifact
	for i, a := range m.Artifacts {
		if a.OS == goos && a.Arch == goarch && isCPUAccel(a.Accel) && cpuFallback == nil {
			cpuFallback = &m.Artifacts[i]
		}
	}
	for _, accel := range accels {
		for _, a := range m.Artifacts {
			if a.OS == goos && a.Arch == goarch && a.Accel == accel {
				return a, nil
			}
		}
	}
	if cpuFallback != nil {
		return *cpuFallback, nil
	}
	return Artifact{}, fmt.Errorf("llamacpp: manifest %s has no build for %s/%s accel=%s", m.RuntimeBuildID, goos, goarch, strings.Join(accels, ">"))
}

func isCPUAccel(accel string) bool { return accel == "cpu" || accel == "cpu-avx2" }

// pickReason explains a pick for the log: which preferred lanes were
// skipped because the manifest does not publish them.
func pickReason(goos, goarch string, accels []string, chosen string) string {
	var skipped []string
	for _, a := range accels {
		if a == chosen {
			break
		}
		if !isCPUAccel(a) {
			skipped = append(skipped, a)
		}
	}
	if len(skipped) == 0 {
		return "preferred"
	}
	return fmt.Sprintf("no %s artifact for %s/%s", strings.Join(skipped, "/"), goos, goarch)
}

// fetchAndUnpack downloads the tarball, verifies its SHA-256, verifies its
// cosign signature per the trust policy, and only then extracts
// llama-server (plus LICENSE/BUILDINFO, and on Windows any *.dll the build
// needs) into dir. The tarball layout is
// llama-server-<tag>-<os>-<arch>-<accel>/{llama-server,LICENSE.llama.cpp,BUILDINFO}
// plus, for lanes that cannot link their GPU runtime statically, DLLs
// beside the binary; entries are extracted by basename to fixed paths, so
// hostile archive paths cannot escape dir.
func (f *Fetcher) fetchAndUnpack(ctx context.Context, a Artifact, dir, buildID string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("llamacpp: mkdir: %w", err)
	}
	// A previous extract's trust stamp must not outlive it: drop it before
	// anything in dir changes so an interrupted re-fetch counts as
	// sha256-only at most.
	_ = os.Remove(filepath.Join(dir, stampVerified))

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
	digest := h.Sum(nil)
	got := hex.EncodeToString(digest)
	if got != a.SHA256 {
		_ = os.Remove(tarball)
		return fmt.Errorf("llamacpp: artifact sha256 mismatch: got %s want %s (refusing to run unverified runtime)", got, a.SHA256)
	}
	// The signature covers the tarball digest, so it is checked here —
	// after the hash matched the manifest and before a single byte is
	// unpacked.
	trust, err := f.verifyArtifactSignature(ctx, a, digest, buildID)
	if err != nil {
		_ = os.Remove(tarball)
		return err
	}
	if err := extractRuntime(tarball, dir); err != nil {
		_ = os.Remove(tarball)
		return err
	}
	_ = os.Remove(tarball)
	// Stamps last: the sha256 stamp's presence means "verified tarball,
	// complete extract"; the trust stamp records under which policy.
	if err := os.WriteFile(filepath.Join(dir, stampVerified), []byte(trust+"\n"), 0o644); err != nil {
		return fmt.Errorf("llamacpp: write trust stamp: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, stampSHA256), []byte(a.SHA256+"\n"), 0o644); err != nil {
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
	dlls := 0
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
		base := filepath.Base(hdr.Name)
		switch {
		case base == binaryName():
			dest, mode, found = filepath.Join(dir, binaryName()), 0o755, true
		case base == "LICENSE.llama.cpp" || base == "BUILDINFO":
			dest, mode = filepath.Join(dir, base), 0o644
		case isWindowsDLL(base):
			// NVIDIA ships no static cuBLAS for Windows, so the CUDA
			// tarball must carry cublas64_12.dll and cublasLt64_12.dll
			// beside the exe; dropping them leaves a binary that cannot
			// start (teraflock/flockd#45, teraflock/runtimes#3). A DLL
			// next to the exe is loaded by that exe, so it is exactly as
			// trusted as the exe — and it is covered by the same tarball
			// sha256 and signature, which is what makes this acceptable.
			if dlls++; dlls > maxRuntimeDLLs {
				return fmt.Errorf("llamacpp: tarball carries more than %d DLLs", maxRuntimeDLLs)
			}
			dest, mode = filepath.Join(dir, base), 0o644
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

// maxRuntimeDLLs bounds what a tarball can drop next to the binary. The
// CUDA lane needs two; the cap is defence in depth behind the sha256 and
// signature checks, not the primary control.
const maxRuntimeDLLs = 16

// isWindowsDLL reports whether a tar member is a DLL this platform should
// keep. Only on Windows: a Linux or macOS node has no use for one, and the
// narrower rule keeps the tarball contract tight.
func isWindowsDLL(base string) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	return strings.EqualFold(filepath.Ext(base), ".dll")
}

func binaryName() string {
	if runtime.GOOS == "windows" {
		return "llama-server.exe"
	}
	return "llama-server"
}
