#!/usr/bin/env bash
# goreleaser post hook for the universal darwin binaries (flockd, tera):
# Developer ID sign with the hardened runtime and an Apple timestamp, then
# notarize the binary (zipped, as the Notary service requires) and wait for
# Apple's verdict. Runs on the Linux release job via rcodesign; works the
# same on a Mac. Policy: scripts/release/lib.sh. teraflock/flockd#26.
#
# Nothing is stapled: a bare Mach-O has nowhere to hold a ticket;
# Gatekeeper checks the notarization online, which is what `brew` and
# `curl | sh` installs need (issue #26, "Design").
#
# Secrets (base64 .p12 + password, App Store Connect API key) are decoded
# into a mode-0700 temp dir that is removed on exit; they never reach argv.
set -euo pipefail
# shellcheck source=lib.sh
. "$(dirname "$0")/lib.sh"

bin=${1:?usage: sign-macos.sh <universal-mach-o>}
name=$(basename "$bin")

if ! have_apple_secrets; then
  skip_or_fail macos "Apple secrets missing: $(missing "${APPLE_SECRETS[@]}")"
fi
require_tool rcodesign "run scripts/release/install-tools.sh"
require_tool zip "apt-get install zip"

umask 077
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
printf '%s' "$APPLE_CERTIFICATE" | base64 -d >"$tmp/cert.p12"
printf '%s' "$APPLE_CERTIFICATE_PASSWORD" >"$tmp/p12.pass"
printf '%s' "$APPLE_API_KEY" | base64 -d >"$tmp/AuthKey.p8"
rcodesign encode-app-store-connect-api-key -o "$tmp/asc.json" \
  "$APPLE_API_ISSUER" "$APPLE_API_KEY_ID" "$tmp/AuthKey.p8" >/dev/null 2>&1

log "macos: signing $name (Developer ID, hardened runtime, timestamped)"
# --for-notarization refuses any setting notarization would reject, so a
# misconfiguration fails here instead of minutes later at Apple.
rcodesign sign \
  --p12-file "$tmp/cert.p12" --p12-password-file "$tmp/p12.pass" \
  --code-signature-flags runtime \
  --for-notarization \
  "$bin"

zip -q -j "$tmp/$name.zip" "$bin"
# Wait limit: Apple's Notary service usually answers in 1-5 minutes, but a
# team's FIRST submissions are much slower — v0.6.2 was still InProgress at
# the old 1500s limit and failed the release. An hour per binary is well
# inside the job's own budget and the wait is what makes the "published =
# notarized" invariant true, so waiting longer beats publishing unsigned.
log "macos: submitting $name for notarization (waiting for Apple)"
rcodesign notary-submit --api-key-file "$tmp/asc.json" \
  --wait --max-wait-seconds "${NOTARY_MAX_WAIT_SECONDS:-3600}" \
  "$tmp/$name.zip"
log "macos: $name signed and notarized"
