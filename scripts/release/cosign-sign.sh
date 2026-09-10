#!/usr/bin/env bash
# goreleaser `signs:` command: a Sigstore bundle over checksums.txt (which
# covers every archive and package by hash). Both custody shapes from
# docs#34 are implemented; COSIGN_MODE (a repository/org variable) picks:
#   keyless   OIDC identity of this workflow via Fulcio; verifiers pin the
#             certificate identity + issuer. Note: the workflow path lands
#             in the public Rekor log for good.
#   kms       a long-lived key in a KMS; COSIGN_KMS_KEY is the cosign KMS
#             URI (awskms:///<key-arn>); the job needs credentials for it
#             (AWS OIDC role, see ci.yml). Verifiers pin the public key.
#   unset     no signature. The workflow passes --skip=sign to goreleaser in
#             this case so no dangling artifact is registered; this branch
#             only guards a direct call.
# cosign v3 writes bundles only (no detached .sig), hence ${artifact}.sigstore.json.
set -euo pipefail
# shellcheck source=lib.sh
. "$(dirname "$0")/lib.sh"

artifact=${1:?usage: cosign-sign.sh <artifact> <bundle-out>}
bundle=${2:?usage: cosign-sign.sh <artifact> <bundle-out>}

set +e
cosign_ready
ready=$?
set -e
case $ready in
1) skip_or_fail cosign "COSIGN_MODE is unset (custody decision docs#34)" ;;
2) exit 1 ;;
esac
require_tool cosign "sigstore/cosign-installer"

case "$COSIGN_MODE" in
keyless)
  log "cosign: keyless (OIDC) bundle for $(basename "$artifact")"
  cosign sign-blob --yes --bundle "$bundle" "$artifact"
  ;;
kms)
  log "cosign: KMS-key bundle for $(basename "$artifact") (key URI kept out of the log)"
  cosign sign-blob --yes --key "$COSIGN_KMS_KEY" --bundle "$bundle" "$artifact"
  ;;
esac
