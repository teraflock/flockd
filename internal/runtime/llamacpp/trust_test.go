package llamacpp

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestKey generates a throwaway P-256 key pair and returns the private
// key plus its PKIX PEM public half (what cosign.pub / the config pin
// hold). Keys are generated per test: no private key is ever committed.
func newTestKey(t *testing.T) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv, pubPEM(t, &priv.PublicKey)
}

func pubPEM(t *testing.T, pub any) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

// signBlob mimics `cosign sign-blob --key`: base64 of the DER ECDSA
// signature over the blob's SHA-256 digest, newline-terminated like the
// .sig files cosign writes.
func signBlob(t *testing.T, priv *ecdsa.PrivateKey, blob []byte) []byte {
	t.Helper()
	digest := sha256.Sum256(blob)
	der, err := ecdsa.SignASN1(rand.Reader, priv, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return []byte(base64.StdEncoding.EncodeToString(der) + "\n")
}

func TestVerifierRoundTrip(t *testing.T) {
	priv, pub := newTestKey(t)
	v, err := ParseVerifier(pub)
	if err != nil {
		t.Fatal(err)
	}
	blob := []byte("a runtime tarball")
	digest := sha256.Sum256(blob)
	sig := signBlob(t, priv, blob)

	if err := v.Verify(digest[:], sig); err != nil {
		t.Fatalf("good signature rejected: %v", err)
	}
	// cosign's .sig ends in a newline and operators paste with whitespace.
	if err := v.Verify(digest[:], []byte("  \t"+strings.TrimSpace(string(sig))+"\r\n")); err != nil {
		t.Errorf("whitespace around the signature must be tolerated: %v", err)
	}

	other := sha256.Sum256([]byte("a different tarball"))
	if err := v.Verify(other[:], sig); err == nil || !strings.Contains(err.Error(), "ECDSA verification failed") {
		t.Errorf("signature over another digest accepted: %v", err)
	}
	// The same signature wrapped the cosign v3 way, with and without the
	// recorded digest, and with whitespace around the JSON.
	for name, bundle := range map[string]string{
		"full":      `{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json","verificationMaterial":{"publicKey":{"hint":"x"}},"messageSignature":{"messageDigest":{"algorithm":"SHA2_256","digest":"` + base64.StdEncoding.EncodeToString(digest[:]) + `"},"signature":"` + strings.TrimSpace(string(sig)) + `"}}`,
		"no digest": `{"messageSignature":{"signature":"` + strings.TrimSpace(string(sig)) + `"}}`,
		"padded":    "\n  {\"messageSignature\":{\"signature\":\"" + strings.TrimSpace(string(sig)) + "\"}}\n",
	} {
		if err := v.Verify(digest[:], []byte(bundle)); err != nil {
			t.Errorf("bundle %s rejected: %v", name, err)
		}
	}
	_, otherPub := newTestKey(t)
	v2, _ := ParseVerifier(otherPub)
	if err := v2.Verify(digest[:], sig); err == nil {
		t.Error("signature accepted under the wrong key")
	}
	for name, bad := range map[string][]byte{
		"empty":                           []byte("\n"),
		"not base64":                      []byte("!!!not-base64!!!"),
		"not DER":                         []byte(base64.StdEncoding.EncodeToString([]byte("garbage"))),
		"tampered":                        []byte("A" + string(sig[1:])),
		"oversize":                        []byte(strings.Repeat("A", maxSignatureBytes+1)),
		"cosign-json":                     []byte(`{"base64Signature":"` + strings.TrimSpace(string(sig)) + `"}`),
		"bundle without messageSignature": []byte(`{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json","dsseEnvelope":{}}`),
		"bundle with bad signature":       []byte(`{"messageSignature":{"signature":"AAAA"}}`),
		"bundle wrong digest algorithm":   []byte(`{"messageSignature":{"messageDigest":{"algorithm":"SHA2_512","digest":"` + base64.StdEncoding.EncodeToString(digest[:]) + `"},"signature":"` + strings.TrimSpace(string(sig)) + `"}}`),
		"bundle foreign digest":           []byte(`{"messageSignature":{"messageDigest":{"algorithm":"SHA2_256","digest":"` + base64.StdEncoding.EncodeToString(other[:]) + `"},"signature":"` + strings.TrimSpace(string(sig)) + `"}}`),
		"not json":                        []byte("{not json"),
	} {
		if err := v.Verify(digest[:], bad); err == nil {
			t.Errorf("%s signature accepted", name)
		}
	}
	if err := v.Verify([]byte("short"), sig); err == nil || !strings.Contains(err.Error(), "want a SHA-256") {
		t.Errorf("non-SHA-256 digest accepted: %v", err)
	}
	var nilV *Verifier
	if err := nilV.Verify(digest[:], sig); err == nil {
		t.Error("nil verifier must refuse")
	}
}

func TestParseVerifierAcceptsOnlyECDSAP256(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	edPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, good := newTestKey(t)

	cases := []struct {
		name string
		pem  []byte
		want string // substring of the error; "" = must parse
	}{
		{"p256", good, ""},
		{"p256 with trailing newline", append(good, '\n', '\n'), ""},
		{"rsa", pubPEM(t, &rsaKey.PublicKey), "unsupported key type"},
		{"ed25519", pubPEM(t, edPub), "unsupported key type"},
		{"p384", pubPEM(t, &p384.PublicKey), "unsupported curve"},
		{"garbage", []byte("not a pem at all"), "no PEM block"},
		{"empty", nil, "no PEM block"},
		{"wrong block type", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{1, 2, 3}}), `want "PUBLIC KEY"`},
		{"bad DER", pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte{1, 2, 3}}), "parse PKIX"},
		{"two keys", append(append([]byte{}, good...), good...), "trailing data"},
	}
	for _, c := range cases {
		v, err := ParseVerifier(c.pem)
		if c.want == "" {
			if err != nil || v == nil {
				t.Errorf("%s: want a verifier, got %v", c.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want error containing %q, got %v", c.name, c.want, err)
		}
	}
}

// The compatibility pin: testdata/cosign/ was produced by a real
// `cosign sign-blob --key` run (see its README.txt). If cosign's key-mode
// output format ever changed, this is the test that would notice.
func TestVerifierMatchesRealCosignOutput(t *testing.T) {
	dir := filepath.Join("testdata", "cosign")
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	v, err := ParseVerifier(read("cosign.pub"))
	if err != nil {
		t.Fatalf("cosign.pub: %v", err)
	}
	blob := read("blob")
	digest := sha256.Sum256(blob)
	tampered := sha256.Sum256(append([]byte("x"), blob...))
	for _, name := range []string{"blob.sig", "blob.sigstore.json"} {
		sig := read(name)
		if err := v.Verify(digest[:], sig); err != nil {
			t.Errorf("%s: real cosign signature rejected: %v", name, err)
		}
		if err := v.Verify(tampered[:], sig); err == nil {
			t.Errorf("%s: cosign signature accepted over a tampered blob", name)
		}
	}
	// The bundle's recorded digest is checked too: swapping it for the
	// tampered digest must fail even though the signature bytes are real.
	var bundle map[string]any
	if err := json.Unmarshal(read("blob.sigstore.json"), &bundle); err != nil {
		t.Fatal(err)
	}
	ms := bundle["messageSignature"].(map[string]any)
	if got := ms["messageDigest"].(map[string]any)["digest"]; got != base64.StdEncoding.EncodeToString(digest[:]) {
		t.Errorf("bundle records digest %v, want sha256(blob)", got)
	}
	ms["messageDigest"] = map[string]any{"algorithm": "SHA2_256", "digest": base64.StdEncoding.EncodeToString(tampered[:])}
	swapped, _ := json.Marshal(bundle)
	if err := v.Verify(digest[:], swapped); err == nil || !strings.Contains(err.Error(), "was made over digest") {
		t.Errorf("bundle with a foreign digest accepted: %v", err)
	}
	// The embedded pin is intentionally empty until docs#34 records the
	// key; when it is populated it must at least parse as P-256.
	if embeddedRuntimeSigningKeyPEM != "" {
		if _, err := ParseVerifier([]byte(embeddedRuntimeSigningKeyPEM)); err != nil {
			t.Fatalf("embedded pin does not parse: %v", err)
		}
	}
}
