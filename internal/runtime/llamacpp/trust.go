package llamacpp

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
)

// Runtime artifact trust (teraflock/flockd#25, SPEC §A1.3 "downloads a
// pinned, signed llama-server build, verifies it").
//
// What is verified: the detached signature teraflock/runtimes publishes
// next to each tarball (manifest field cosign_signature_url) and, when it
// exists, `<manifest-url>.sig` over the manifest body. Both are produced
// by `cosign sign-blob --key <KMS or file key>`, whose signature is an
// ASN.1/DER ECDSA signature over the SHA-256 digest of the blob. cosign
// v2 wrote it as a bare base64 line (`--output-signature x.sig`); cosign
// v3 wraps the same bytes in a Sigstore bundle JSON (`--bundle
// x.sigstore.json`, messageSignature.signature) alongside the digest it
// signed and transparency-log material. Either shape is accepted at the
// URL, detected by its first byte. The signature itself is exactly what
// the Go standard library verifies with crypto/ecdsa.VerifyASN1 +
// crypto/x509.ParsePKIXPublicKey, so no sigstore dependency is needed:
// verification is offline, has no new module surface, and still builds
// with CGO_ENABLED=0 for every release target. testdata/cosign/ holds a
// real cosign-produced fixture of both shapes that pins the format
// compatibility in TestVerifierMatchesRealCosignOutput.
//
// What is NOT covered: keyless (Fulcio certificate + Rekor transparency
// log) signatures. Verifying those safely means walking the Fulcio chain,
// checking the certificate's OIDC identity and validating the Rekor
// integrated time against the certificate's validity window; that cannot
// be hand-rolled responsibly. If SPEC §A3 Key custody (teraflock/docs#34)
// picks keyless instead of the recommended KMS-held P-256 key, the
// follow-up is github.com/sigstore/sigstore-go bundle verification with
// an embedded trusted root. Artifact.CosignCertificateURL is modelled for
// that future (the runtimes manifest schema already defines it) and is
// deliberately unused here: with a pinned public key there is no
// certificate to fetch, and fetching one from the artifact host would
// only ever prove what the host chose to publish.

// embeddedRuntimeSigningKeyPEM is the daemon's pin: the PKIX PEM public key
// that Teraflock runtime artifacts are signed with. This is the public half
// of the AWS KMS key recorded in SPEC §A3.1 Key custody
// (alias/teraflock-artifact-signing, decided in teraflock/docs#34); only the
// flockd and runtimes release workflows can sign with the private half, and
// only on a tag. Never populated from the manifest or the artifact host: an
// attacker who controls those controls whatever key they would serve.
//
// Rotating this is a daemon release, by design (SPEC §A3.1: there is no
// revocation list; a compromised key means a new pin here plus a
// ConfigUpdate.minimum_version drain of older nodes).
//
// config.Runtime.RequireSignature still defaults to false: teraflock/runtimes
// does not publish signatures next to its tarballs yet (runtimes#1), so
// requiring them would break every node's runtime fetch. Flipping that
// default is stage 2 of #25 and waits on runtimes#1.
// A var, not a const, only so tests can model a daemon built before the
// pin existed; nothing outside _test.go writes it.
var embeddedRuntimeSigningKeyPEM = `-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEW24PNaVshWhqlboDdqx0SjJJF5QG
vl4fl1HGx8Aw+Zu6FsrrXVYveDsizVLBYJrujeIxnnblvIn6c04vx4iJVg==
-----END PUBLIC KEY-----
`

// Verifier checks cosign key-mode blob signatures against one pinned
// ECDSA P-256 public key.
type Verifier struct {
	pub *ecdsa.PublicKey
}

// ParseVerifier parses a PEM-encoded PKIX public key (the cosign.pub
// format, "-----BEGIN PUBLIC KEY-----") and returns a verifier for it.
// Only ECDSA over P-256 is accepted, matching cosign's default
// ecdsa-sha2-256-nistp256 signing algorithm and the docs#34
// recommendation; every other key type is rejected with a clear error so
// a misconfigured pin fails at startup, not at the first fetch.
func ParseVerifier(pemBytes []byte) (*Verifier, error) {
	block, rest := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("llamacpp: signing key: no PEM block found")
	}
	if strings.TrimSpace(string(rest)) != "" {
		return nil, errors.New("llamacpp: signing key: trailing data after the PEM block (expected exactly one PUBLIC KEY)")
	}
	if block.Type != "PUBLIC KEY" {
		return nil, fmt.Errorf("llamacpp: signing key: PEM block is %q, want \"PUBLIC KEY\" (PKIX; cosign.pub format)", block.Type)
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("llamacpp: signing key: parse PKIX public key: %w", err)
	}
	pub, ok := key.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("llamacpp: signing key: unsupported key type %T, want ECDSA P-256", key)
	}
	if pub.Curve != elliptic.P256() {
		return nil, fmt.Errorf("llamacpp: signing key: unsupported curve %s, want P-256", pub.Params().Name)
	}
	return &Verifier{pub: pub}, nil
}

// maxSignatureBytes bounds a fetched .sig: a base64 P-256 DER signature
// is under 100 bytes, so anything near the cap is not a signature.
const maxSignatureBytes = 64 << 10

// Verify checks sig — the contents of a cosign_signature_url: either a
// bare base64 ASN.1/DER ECDSA signature (cosign v2 .sig, surrounding
// whitespace tolerated) or a Sigstore bundle JSON (cosign v3, first
// non-space byte '{') — over blobSHA256, the SHA-256 digest of the signed
// blob. cosign sign-blob signs the digest, so callers pass the same hash
// they already computed for the sha256 pin — never the blob itself. A
// bundle that records a different digest than the one being verified is
// rejected before the signature is even looked at.
func (v *Verifier) Verify(blobSHA256 []byte, sig []byte) error {
	if v == nil || v.pub == nil {
		return errors.New("llamacpp: no signing key pinned")
	}
	if len(blobSHA256) != sha256.Size {
		return fmt.Errorf("llamacpp: signature check: digest is %d bytes, want a SHA-256", len(blobSHA256))
	}
	if len(sig) > maxSignatureBytes {
		return fmt.Errorf("llamacpp: signature check: .sig is %d bytes, not a signature", len(sig))
	}
	der, err := decodeSignature(blobSHA256, sig)
	if err != nil {
		return fmt.Errorf("llamacpp: signature check: %w", err)
	}
	if !ecdsa.VerifyASN1(v.pub, blobSHA256, der) {
		return errors.New("llamacpp: signature check: ECDSA verification failed (signature does not match the pinned key over this digest)")
	}
	return nil
}

// sigstoreBundle is the slice of a Sigstore bundle
// (application/vnd.dev.sigstore.bundle.v0.3+json) that key-mode
// verification needs. verificationMaterial (public-key hint, Rekor entry,
// RFC 3161 timestamp) is ignored: the pinned key is the trust anchor.
type sigstoreBundle struct {
	MessageSignature *struct {
		MessageDigest *struct {
			Algorithm string `json:"algorithm"`
			Digest    string `json:"digest"`
		} `json:"messageDigest"`
		Signature string `json:"signature"`
	} `json:"messageSignature"`
}

// decodeSignature returns the DER signature carried by sig in either
// accepted shape, after checking a bundle's recorded digest against
// blobSHA256.
func decodeSignature(blobSHA256, sig []byte) ([]byte, error) {
	trimmed := strings.TrimSpace(string(sig))
	if trimmed == "" {
		return nil, errors.New(".sig is empty")
	}
	b64 := trimmed
	if trimmed[0] == '{' {
		var bundle sigstoreBundle
		if err := json.Unmarshal([]byte(trimmed), &bundle); err != nil {
			return nil, fmt.Errorf("sigstore bundle: %w", err)
		}
		if bundle.MessageSignature == nil || bundle.MessageSignature.Signature == "" {
			return nil, errors.New("sigstore bundle has no messageSignature.signature (DSSE/attestation bundles are not runtime signatures)")
		}
		if md := bundle.MessageSignature.MessageDigest; md != nil && md.Digest != "" {
			if md.Algorithm != "" && md.Algorithm != "SHA2_256" {
				return nil, fmt.Errorf("sigstore bundle digest algorithm %q, want SHA2_256", md.Algorithm)
			}
			recorded, err := base64.StdEncoding.DecodeString(md.Digest)
			if err != nil {
				return nil, fmt.Errorf("sigstore bundle digest is not base64: %w", err)
			}
			if !bytes.Equal(recorded, blobSHA256) {
				return nil, fmt.Errorf("sigstore bundle was made over digest %x, not this artifact's %x", recorded, blobSHA256)
			}
		}
		b64 = bundle.MessageSignature.Signature
	}
	der, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("signature is not base64: %w", err)
	}
	return der, nil
}
