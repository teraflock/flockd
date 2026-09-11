package llamacpp

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// sigOpts shapes a signed runtimes host for the trust-policy tests.
type sigOpts struct {
	advertise bool              // manifest carries cosign_signature_url
	artKey    *ecdsa.PrivateKey // signs the tarball .sig (nil = route 404s)
	artSig    []byte            // literal .sig body, overrides artKey
	manKey    *ecdsa.PrivateKey // signs manifest.json.sig (nil = absent)
	manSig    []byte            // literal manifest .sig body, overrides manKey
	manStatus int               // status for an absent manifest .sig (0 = 404)
}

// serveSigned serves manifest.json, the tarball, its .sig and
// manifest.json.sig per opts. The manifest is marshalled once so the
// manifest signature covers the exact bytes served. Returns the server,
// the manifest URL and a counter of tarball downloads.
func serveSigned(t *testing.T, tarball []byte, o sigOpts) (*httptest.Server, string, *atomic.Int32) {
	t.Helper()
	var downloads atomic.Int32
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	sum := sha256.Sum256(tarball)
	art := Artifact{
		OS: runtime.GOOS, Arch: runtime.GOARCH, Accel: "metal",
		URL: srv.URL + "/llama-server.tar.gz", SHA256: hex.EncodeToString(sum[:]),
		Size: int64(len(tarball)),
	}
	if o.advertise {
		art.CosignSignatureURL = srv.URL + "/llama-server.tar.gz.sig"
	}
	manJSON, err := json.Marshal(ArtifactManifest{RuntimeBuildID: "llamacpp-b9999-1", Artifacts: []Artifact{art}})
	if err != nil {
		t.Fatal(err)
	}

	artSig := o.artSig
	if artSig == nil && o.artKey != nil {
		artSig = signBlob(t, o.artKey, tarball)
	}
	manSig := o.manSig
	if manSig == nil && o.manKey != nil {
		manSig = signBlob(t, o.manKey, manJSON)
	}
	manStatus := o.manStatus
	if manStatus == 0 {
		manStatus = http.StatusNotFound
	}

	mux.HandleFunc("/manifest.json", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(manJSON) })
	mux.HandleFunc("/manifest.json.sig", func(w http.ResponseWriter, _ *http.Request) {
		if manSig == nil {
			w.WriteHeader(manStatus)
			return
		}
		_, _ = w.Write(manSig)
	})
	mux.HandleFunc("/llama-server.tar.gz", func(w http.ResponseWriter, _ *http.Request) {
		downloads.Add(1)
		_, _ = w.Write(tarball)
	})
	mux.HandleFunc("/llama-server.tar.gz.sig", func(w http.ResponseWriter, _ *http.Request) {
		if artSig == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(artSig)
	})
	return srv, srv.URL + "/manifest.json", &downloads
}

func testLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

func readTrust(t *testing.T, cacheDir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(cacheDir, "llamacpp-b9999-1", stampVerified))
	if err != nil {
		t.Fatalf("trust stamp: %v", err)
	}
	return strings.TrimSpace(string(b))
}

func assertNotExtracted(t *testing.T, cacheDir string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(cacheDir, "llamacpp-b9999-1", binaryName())); err == nil {
		t.Fatal("unverified binary was extracted")
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "llamacpp-b9999-1", ".download.tar.gz")); err == nil {
		t.Fatal("staged tarball left behind after refusal")
	}
}

func TestSignatureVerifiedUnderBothPolicies(t *testing.T) {
	priv, pub := newTestKey(t)
	tarball := makeTarball(t, []byte("#!/bin/sh\necho signed\n"))
	for _, require := range []bool{false, true} {
		_, manifestURL, _ := serveSigned(t, tarball, sigOpts{advertise: true, artKey: priv, manKey: priv})
		log, logs := testLogger()
		dir := t.TempDir()
		f := &Fetcher{ManifestURL: manifestURL, CacheDir: dir, SigningKeyPEM: pub, RequireSignature: require, Log: log}
		if _, id, err := f.Ensure(context.Background(), "metal"); err != nil || id != "llamacpp-b9999-1" {
			t.Fatalf("require=%v: %v", require, err)
		}
		if got := readTrust(t, dir); got != trustCosign {
			t.Errorf("require=%v: trust stamp = %q, want cosign", require, got)
		}
		if !strings.Contains(logs.String(), "runtime signature verified") || !strings.Contains(logs.String(), "runtime_build_id=llamacpp-b9999-1") {
			t.Errorf("require=%v: missing success log line:\n%s", require, logs.String())
		}
		if strings.Contains(logs.String(), "level=WARN") {
			t.Errorf("require=%v: unexpected warning:\n%s", require, logs.String())
		}
		// Preflight agrees.
		if err := f.Preflight(context.Background(), "metal"); err != nil {
			t.Errorf("require=%v: preflight: %v", require, err)
		}
	}
}

// cosign v3 publishes a Sigstore bundle rather than a bare .sig; the
// manifest still points at it via cosign_signature_url.
func TestSignatureAcceptsBundleAtSignatureURL(t *testing.T) {
	priv, pub := newTestKey(t)
	tarball := makeTarball(t, []byte("#!/bin/sh\necho bundled\n"))
	sum := sha256.Sum256(tarball)
	bundle := `{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json","verificationMaterial":{"publicKey":{"hint":"x"},"tlogEntries":[]},"messageSignature":{"messageDigest":{"algorithm":"SHA2_256","digest":"` +
		base64.StdEncoding.EncodeToString(sum[:]) + `"},"signature":"` + strings.TrimSpace(string(signBlob(t, priv, tarball))) + `"}}`
	_, manifestURL, _ := serveSigned(t, tarball, sigOpts{advertise: true, artSig: []byte(bundle), manKey: priv})
	dir := t.TempDir()
	f := &Fetcher{ManifestURL: manifestURL, CacheDir: dir, SigningKeyPEM: pub, RequireSignature: true}
	if _, _, err := f.Ensure(context.Background(), "metal"); err != nil {
		t.Fatalf("bundle-shaped signature rejected: %v", err)
	}
	if got := readTrust(t, dir); got != trustCosign {
		t.Errorf("trust stamp = %q, want cosign", got)
	}

	// A bundle for a different tarball (digest mismatch) is refused even
	// when its signature is genuine.
	otherTarball := makeTarball(t, []byte("#!/bin/sh\necho other\n"))
	otherSum := sha256.Sum256(otherTarball)
	foreign := `{"messageSignature":{"messageDigest":{"algorithm":"SHA2_256","digest":"` + base64.StdEncoding.EncodeToString(otherSum[:]) +
		`"},"signature":"` + strings.TrimSpace(string(signBlob(t, priv, otherTarball))) + `"}}`
	_, manifestURL2, _ := serveSigned(t, tarball, sigOpts{advertise: true, artSig: []byte(foreign)})
	dir2 := t.TempDir()
	_, _, err := (&Fetcher{ManifestURL: manifestURL2, CacheDir: dir2, SigningKeyPEM: pub}).Ensure(context.Background(), "metal")
	if err == nil || !strings.Contains(err.Error(), "was made over digest") {
		t.Fatalf("want digest-mismatch refusal, got %v", err)
	}
	assertNotExtracted(t, dir2)
}

// cosign v3 `sign-blob --bundle` with --tlog-upload=false writes its own
// key-mode bundle, {"base64Signature": ...} and nothing else — the shape
// runtimes llamacpp-b9892-4 published to stable, and the one a daemon
// with require_signature=true refused until it was accepted here.
func TestSignatureAcceptsCosignKeyModeBundle(t *testing.T) {
	priv, pub := newTestKey(t)
	tarball := makeTarball(t, []byte("#!/bin/sh\necho key-mode\n"))
	bundle := `{"base64Signature":"` + strings.TrimSpace(string(signBlob(t, priv, tarball))) + `"}`
	_, manifestURL, _ := serveSigned(t, tarball, sigOpts{advertise: true, artSig: []byte(bundle), manKey: priv})
	dir := t.TempDir()
	f := &Fetcher{ManifestURL: manifestURL, CacheDir: dir, SigningKeyPEM: pub, RequireSignature: true}
	if _, _, err := f.Ensure(context.Background(), "metal"); err != nil {
		t.Fatalf("cosign key-mode bundle rejected: %v", err)
	}
	if got := readTrust(t, dir); got != trustCosign {
		t.Errorf("trust stamp = %q, want cosign", got)
	}

	// The same shape over a different blob still fails: no digest is
	// recorded, so the ECDSA check itself has to catch it.
	other := makeTarball(t, []byte("#!/bin/sh\necho other\n"))
	wrong := `{"base64Signature":"` + strings.TrimSpace(string(signBlob(t, priv, other))) + `"}`
	_, manifestURL2, _ := serveSigned(t, tarball, sigOpts{advertise: true, artSig: []byte(wrong), manKey: priv})
	_, _, err := (&Fetcher{ManifestURL: manifestURL2, CacheDir: t.TempDir(), SigningKeyPEM: pub, RequireSignature: true}).Ensure(context.Background(), "metal")
	if err == nil || !strings.Contains(err.Error(), "ECDSA verification failed") {
		t.Fatalf("want ECDSA refusal for a key-mode bundle over another blob, got %v", err)
	}

	// Ambiguity is refused rather than guessed.
	both := `{"base64Signature":"AA==","messageSignature":{"signature":"AA=="}}`
	if _, err := decodeSignature(make([]byte, 32), []byte(both)); err == nil || !strings.Contains(err.Error(), "both") {
		t.Fatalf("want refusal of a bundle carrying both shapes, got %v", err)
	}
}

func TestSignatureRejectsBadSignature(t *testing.T) {
	priv, pub := newTestKey(t)
	tarball := makeTarball(t, []byte("#!/bin/sh\necho evil\n"))
	other := signBlob(t, priv, []byte("some other tarball"))
	cases := map[string]sigOpts{
		"garbage .sig":         {advertise: true, artSig: []byte("not a signature\n")},
		"signature over other": {advertise: true, artSig: other},
		"advertised but 404":   {advertise: true},
	}
	for name, o := range cases {
		_, manifestURL, _ := serveSigned(t, tarball, o)
		dir := t.TempDir()
		f := &Fetcher{ManifestURL: manifestURL, CacheDir: dir, SigningKeyPEM: pub}
		_, _, err := f.Ensure(context.Background(), "metal")
		if err == nil || !strings.Contains(err.Error(), "artifact signature invalid") || !strings.Contains(err.Error(), "refusing to run unverified runtime") {
			t.Fatalf("%s: want signature error, got %v", name, err)
		}
		assertNotExtracted(t, dir)
	}
}

func TestSignatureRejectsWrongKey(t *testing.T) {
	signer, _ := newTestKey(t)
	_, pinned := newTestKey(t)
	tarball := makeTarball(t, []byte("#!/bin/sh\necho wrong key\n"))
	_, manifestURL, _ := serveSigned(t, tarball, sigOpts{advertise: true, artKey: signer})
	dir := t.TempDir()
	_, _, err := (&Fetcher{ManifestURL: manifestURL, CacheDir: dir, SigningKeyPEM: pinned}).Ensure(context.Background(), "metal")
	if err == nil || !strings.Contains(err.Error(), "artifact signature invalid") {
		t.Fatalf("want wrong-key error, got %v", err)
	}
	assertNotExtracted(t, dir)
}

func TestSignatureRejectsUnparseablePin(t *testing.T) {
	priv, _ := newTestKey(t)
	tarball := makeTarball(t, []byte("#!/bin/sh\n"))
	_, manifestURL, _ := serveSigned(t, tarball, sigOpts{advertise: true, artKey: priv})
	f := &Fetcher{ManifestURL: manifestURL, CacheDir: t.TempDir(), SigningKeyPEM: []byte("garbage")}
	if _, _, err := f.Ensure(context.Background(), "metal"); err == nil || !strings.Contains(err.Error(), "signing key") {
		t.Fatalf("want pin parse error, got %v", err)
	}
	if err := f.Preflight(context.Background(), "metal"); err == nil || !strings.Contains(err.Error(), "signing key") {
		t.Fatalf("preflight: want pin parse error, got %v", err)
	}
}

// A signed catalog reaching a daemon without a pin (the state before
// docs#34 records the key): trusted by sha256 with one warning, unless
// the operator demanded signatures.
func TestSignatureAdvertisedButNoVerifier(t *testing.T) {
	withoutEmbeddedPin(t)
	priv, _ := newTestKey(t)
	tarball := makeTarball(t, []byte("#!/bin/sh\necho unpinned\n"))
	_, manifestURL, _ := serveSigned(t, tarball, sigOpts{advertise: true, artKey: priv, manKey: priv})

	log, logs := testLogger()
	dir := t.TempDir()
	f := &Fetcher{ManifestURL: manifestURL, CacheDir: dir, Log: log}
	if _, _, err := f.Ensure(context.Background(), "metal"); err != nil {
		t.Fatalf("stage 1 without a pin must still run: %v", err)
	}
	if got := readTrust(t, dir); got != trustSHA256Only {
		t.Errorf("trust stamp = %q, want sha256-only", got)
	}
	if n := strings.Count(logs.String(), "no verifier is pinned"); n != 1 {
		t.Errorf("want exactly one no-verifier warning, got %d:\n%s", n, logs.String())
	}
	// A second fetch through the same Fetcher must not warn again.
	if err := os.RemoveAll(filepath.Join(dir, "llamacpp-b9999-1")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.Ensure(context.Background(), "metal"); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(logs.String(), "no verifier is pinned"); n != 1 {
		t.Errorf("warning must be logged once per Fetcher, got %d", n)
	}

	dir2 := t.TempDir()
	strict := &Fetcher{ManifestURL: manifestURL, CacheDir: dir2, RequireSignature: true}
	_, _, err := strict.Ensure(context.Background(), "metal")
	if err == nil || !strings.Contains(err.Error(), "no runtime signing key is pinned") {
		t.Fatalf("require_signature without a pin must refuse, got %v", err)
	}
	assertNotExtracted(t, dir2)
	if err := strict.Preflight(context.Background(), "metal"); err == nil || !strings.Contains(err.Error(), "no runtime signing key is pinned") {
		t.Fatalf("preflight: want no-pin error, got %v", err)
	}
}

func TestNoSignatureAdvertisedUnderBothPolicies(t *testing.T) {
	priv, pub := newTestKey(t)
	tarball := makeTarball(t, []byte("#!/bin/sh\necho old manifest\n"))
	// Old-style manifest: no cosign_signature_url even though the host
	// happens to have a .sig — the daemon must not guess URLs. The
	// manifest itself is signed so the strict case reaches the artifact
	// rule rather than stopping at "manifest has no signature".
	_, manifestURL, _ := serveSigned(t, tarball, sigOpts{advertise: false, artKey: priv, manKey: priv})

	log, logs := testLogger()
	dir := t.TempDir()
	f := &Fetcher{ManifestURL: manifestURL, CacheDir: dir, SigningKeyPEM: pub, Log: log}
	if _, _, err := f.Ensure(context.Background(), "metal"); err != nil {
		t.Fatalf("unsigned manifest under stage 1 must run: %v", err)
	}
	if got := readTrust(t, dir); got != trustSHA256Only {
		t.Errorf("trust stamp = %q, want sha256-only", got)
	}
	if !strings.Contains(logs.String(), "advertises no cosign signature") {
		t.Errorf("want an unsigned warning:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "runtime signature verified") {
		t.Error("nothing was verified; success line must not appear")
	}

	dir2 := t.TempDir()
	strict := &Fetcher{ManifestURL: manifestURL, CacheDir: dir2, SigningKeyPEM: pub, RequireSignature: true, Log: log}
	_, _, err := strict.Ensure(context.Background(), "metal")
	if err == nil || !strings.Contains(err.Error(), "advertises no cosign signature") || !strings.Contains(err.Error(), "runtime.require_signature=true") {
		t.Fatalf("want refuse-unsigned error, got %v", err)
	}
	assertNotExtracted(t, dir2)
	// tera up's preflight reports the same refusal before installing.
	if err := strict.Preflight(context.Background(), "metal"); err == nil || !strings.Contains(err.Error(), "advertises no cosign signature") {
		t.Fatalf("preflight: want refuse-unsigned error, got %v", err)
	}
}

func TestManifestSignaturePolicy(t *testing.T) {
	priv, pub := newTestKey(t)
	imposter, _ := newTestKey(t)
	tarball := makeTarball(t, []byte("#!/bin/sh\necho manifest\n"))

	cases := []struct {
		name    string
		o       sigOpts
		require bool
		wantErr string // "" = must succeed
	}{
		{"good, lenient", sigOpts{advertise: true, artKey: priv, manKey: priv}, false, ""},
		{"good, strict", sigOpts{advertise: true, artKey: priv, manKey: priv}, true, ""},
		{"bad, lenient", sigOpts{advertise: true, artKey: priv, manSig: []byte("nope\n")}, false, "manifest signature invalid"},
		{"bad, strict", sigOpts{advertise: true, artKey: priv, manSig: []byte("nope\n")}, true, "manifest signature invalid"},
		{"wrong key, lenient", sigOpts{advertise: true, artKey: priv, manKey: imposter}, false, "manifest signature invalid"},
		{"absent 404, lenient", sigOpts{advertise: true, artKey: priv}, false, ""},
		{"absent 403 (S3), lenient", sigOpts{advertise: true, artKey: priv, manStatus: http.StatusForbidden}, false, ""},
		{"absent 404, strict", sigOpts{advertise: true, artKey: priv}, true, "has no signature"},
		{"absent 403 (S3), strict", sigOpts{advertise: true, artKey: priv, manStatus: http.StatusForbidden}, true, "has no signature"},
		{"absent 500, lenient", sigOpts{advertise: true, artKey: priv, manStatus: http.StatusInternalServerError}, false, "unexpected status"},
	}
	for _, c := range cases {
		_, manifestURL, downloads := serveSigned(t, tarball, c.o)
		dir := t.TempDir()
		f := &Fetcher{ManifestURL: manifestURL, CacheDir: dir, SigningKeyPEM: pub, RequireSignature: c.require}
		_, _, err := f.Ensure(context.Background(), "metal")
		if c.wantErr == "" {
			if err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: want error containing %q, got %v", c.name, c.wantErr, err)
		}
		// A refused manifest never leads to a tarball download.
		if downloads.Load() != 0 {
			t.Errorf("%s: tarball downloaded despite manifest refusal", c.name)
		}
		if perr := f.Preflight(context.Background(), "metal"); perr == nil || !strings.Contains(perr.Error(), c.wantErr) {
			t.Errorf("%s: preflight want %q, got %v", c.name, c.wantErr, perr)
		}
	}
}

// Without a pin the manifest .sig is never even requested: there is
// nothing to check it against, and guessing keys is exactly what the
// design forbids.
func TestManifestSignatureSkippedWithoutVerifier(t *testing.T) {
	withoutEmbeddedPin(t)
	priv, _ := newTestKey(t)
	tarball := makeTarball(t, []byte("#!/bin/sh\n"))
	_, manifestURL, _ := serveSigned(t, tarball, sigOpts{advertise: false, artKey: priv, manSig: []byte("would fail if checked\n")})
	if _, _, err := (&Fetcher{ManifestURL: manifestURL, CacheDir: t.TempDir()}).Ensure(context.Background(), "metal"); err != nil {
		t.Fatalf("no pin means no manifest signature check: %v", err)
	}
}

// The policy-upgrade path: an extract cached under sha256-only (stage 1,
// or a pre-#25 daemon that wrote no trust stamp at all) is re-fetched
// and signature-verified once require_signature is on; a cosign stamp
// is reused without touching the network for the tarball.
// withoutEmbeddedPin models a daemon built before SPEC §A3.1 recorded the
// runtime signing key, so "no pin configured" can still be exercised now
// that release binaries carry one.
func withoutEmbeddedPin(t *testing.T) {
	t.Helper()
	saved := embeddedRuntimeSigningKeyPEM
	embeddedRuntimeSigningKeyPEM = ""
	t.Cleanup(func() { embeddedRuntimeSigningKeyPEM = saved })
}

func TestCachedReuseHonoursTrustPolicy(t *testing.T) {
	withoutEmbeddedPin(t)
	priv, pub := newTestKey(t)
	tarball := makeTarball(t, []byte("#!/bin/sh\necho cached\n"))
	_, manifestURL, downloads := serveSigned(t, tarball, sigOpts{advertise: true, artKey: priv, manKey: priv})
	dir := t.TempDir()
	buildDir := filepath.Join(dir, "llamacpp-b9999-1")

	// 1. Stage-1 daemon without a pin: sha256-only extract.
	if _, _, err := (&Fetcher{ManifestURL: manifestURL, CacheDir: dir}).Ensure(context.Background(), "metal"); err != nil {
		t.Fatal(err)
	}
	if got := readTrust(t, dir); got != trustSHA256Only || downloads.Load() != 1 {
		t.Fatalf("stage 1: trust=%q downloads=%d", got, downloads.Load())
	}

	// 2. Same policy, cached: no download.
	if _, _, err := (&Fetcher{ManifestURL: manifestURL, CacheDir: dir}).Ensure(context.Background(), "metal"); err != nil {
		t.Fatal(err)
	}
	if downloads.Load() != 1 {
		t.Fatalf("sha256-only cache not reused under lenient policy: downloads=%d", downloads.Load())
	}

	// 3. Pre-#25 daemons wrote no trust stamp: counts as sha256-only and
	//    is likewise fine under the lenient policy…
	if err := os.Remove(filepath.Join(buildDir, stampVerified)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (&Fetcher{ManifestURL: manifestURL, CacheDir: dir, SigningKeyPEM: pub}).Ensure(context.Background(), "metal"); err != nil {
		t.Fatal(err)
	}
	if downloads.Load() != 1 {
		t.Fatalf("stampless cache not reused under lenient policy: downloads=%d", downloads.Load())
	}

	// 4. …but require_signature re-fetches and verifies it.
	log, logs := testLogger()
	strict := &Fetcher{ManifestURL: manifestURL, CacheDir: dir, SigningKeyPEM: pub, RequireSignature: true, Log: log}
	if _, _, err := strict.Ensure(context.Background(), "metal"); err != nil {
		t.Fatal(err)
	}
	if got := readTrust(t, dir); got != trustCosign || downloads.Load() != 2 {
		t.Fatalf("policy upgrade: trust=%q downloads=%d, want cosign/2", got, downloads.Load())
	}
	if !strings.Contains(logs.String(), "re-fetching to verify its signature") || !strings.Contains(logs.String(), "runtime signature verified") {
		t.Errorf("upgrade path logs:\n%s", logs.String())
	}

	// 5. A cosign stamp satisfies the strict policy: reused, no download.
	if _, _, err := strict.Ensure(context.Background(), "metal"); err != nil {
		t.Fatal(err)
	}
	if downloads.Load() != 2 {
		t.Fatalf("cosign cache not reused under strict policy: downloads=%d", downloads.Load())
	}
	// A stamp that claims more than it should ("cosign" without the
	// binary) still does not short-circuit: the binary must exist.
	if err := os.Remove(filepath.Join(buildDir, binaryName())); err != nil {
		t.Fatal(err)
	}
	if _, _, err := strict.Ensure(context.Background(), "metal"); err != nil {
		t.Fatal(err)
	}
	if downloads.Load() != 3 {
		t.Fatalf("missing binary must trigger a re-fetch: downloads=%d", downloads.Load())
	}
}

// Manifest decode: the two cosign fields round-trip and old manifests
// without them decode to empty strings (not an error).
func TestManifestDecodesCosignFields(t *testing.T) {
	cases := []struct {
		name string
		body string
		sig  string
		cert string
	}{
		{"signed", `{"runtime_build_id":"llamacpp-b1-1","artifacts":[{"os":"linux","arch":"amd64","accel":"cpu-avx2","url":"https://h/a.tar.gz","sha256":"00","size_bytes":1,"cosign_signature_url":"https://h/a.tar.gz.sig","cosign_certificate_url":"https://h/a.tar.gz.pem"}]}`, "https://h/a.tar.gz.sig", "https://h/a.tar.gz.pem"},
		{"unsigned", `{"runtime_build_id":"llamacpp-b1-1","artifacts":[{"os":"linux","arch":"amd64","accel":"cpu-avx2","url":"https://h/a.tar.gz","sha256":"00","size_bytes":1}]}`, "", ""},
		{"extra fields ignored", `{"runtime_build_id":"llamacpp-b1-1","runtime":"llamacpp","upstream_tag":"b1","artifacts":[{"os":"linux","arch":"amd64","accel":"cpu-avx2","filename":"a.tar.gz","url":"https://h/a.tar.gz","sha256":"00","size_bytes":1,"cosign_signature_url":"https://h/a.tar.gz.sig"}]}`, "https://h/a.tar.gz.sig", ""},
	}
	for _, c := range cases {
		var m ArtifactManifest
		if err := json.Unmarshal([]byte(c.body), &m); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(m.Artifacts) != 1 || m.Artifacts[0].CosignSignatureURL != c.sig || m.Artifacts[0].CosignCertificateURL != c.cert {
			t.Errorf("%s: decoded %+v", c.name, m.Artifacts)
		}
	}
}
