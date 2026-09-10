Test-only cosign fixture (no secret material here).

Proves that flockd's stdlib verifier (internal/runtime/llamacpp/trust.go)
accepts what a real `cosign sign-blob --key` run emits, in both shapes a
cosign_signature_url may point at:

  blob.sigstore.json  Sigstore bundle (cosign v3 default output):
                      messageSignature.signature is the base64 ASN.1/DER
                      ECDSA P-256 signature over the blob's SHA-256, and
                      messageSignature.messageDigest.digest is that
                      SHA-256, base64.
  blob.sig            The raw cosign v2-style detached signature: the same
                      base64 DER signature, one line.

Produced with cosign v3.1.3 in a scratch directory:

  COSIGN_PASSWORD='' cosign generate-key-pair
  printf 'teraflock runtime test blob\n' > blob
  COSIGN_PASSWORD='' cosign sign-blob --yes --key cosign.key \
      --bundle blob.sigstore.json blob
  jq -r .messageSignature.signature blob.sigstore.json > blob.sig

Checked with cosign itself before committing:

  cosign verify-blob --key cosign.pub --bundle blob.sigstore.json blob
      # "Verified OK"
  cosign verify-blob --key cosign.pub --signature blob.sig \
      --insecure-ignore-tlog blob
      # "Verified OK" (the raw .sig carries no transparency-log entry)

Only cosign.pub, blob, blob.sig and blob.sigstore.json are committed. The
private key (cosign.key) was generated for this fixture alone, never used
for a release, and was discarded. The bundle's tlogEntries reference the
public Rekor log entry cosign made for this throwaway key; flockd ignores
that section. This key is NOT the Teraflock runtime signing key: the real
pin comes from SPEC §A3 Key custody (teraflock/docs#34).
