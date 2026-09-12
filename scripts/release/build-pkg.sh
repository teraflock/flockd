#!/usr/bin/env bash
# Build the macOS installer package for a release and sign, notarize and
# staple it (teraflock/flockd#47).
#
# Why a .pkg at all: a bare Mach-O cannot hold a notarization ticket, so
# `brew`/tarball installs of the signed binaries still needed Gatekeeper's
# online first-launch check — and flockd's first launch happens inside
# launchd, where the consent dialog is easy to miss and the node is simply
# down until someone clicks it. Files placed by an installer package carry
# no quarantine attribute and the package itself is stapled, so there is
# no online check and no dialog, for the install or for the daemon.
#
# Usage: build-pkg.sh <version> <flockd> <tera> <out.pkg>
#   <flockd>/<tera>  the universal binaries from the release tarball — already
#                    Developer ID signed and notarized by sign-macos.sh
#   <out.pkg>        written only when signing succeeds (or when the pkg leg
#                    is skipped, in which case nothing is written and the
#                    exit is 0; SIGNING_REQUIRED=pkg turns that into failure)
#
# Runs on macOS only (pkgbuild). Signing/notarizing/stapling use rcodesign,
# the same pinned tool as the binaries, so the Installer certificate is a
# .p12 in the environment and never touches a keychain. Policy: lib.sh.
set -euo pipefail
# shellcheck source=lib.sh
. "$(dirname "$0")/lib.sh"

version=${1:?usage: build-pkg.sh <version> <flockd> <tera> <out.pkg>}
flockd_bin=${2:?}
tera_bin=${3:?}
out=${4:?}

# Identifier and install root are the public contract of the package:
# `uninstall pkgutil:` in the cask and `pkgutil --forget` for humans key on
# the identifier; /usr/local/bin is on every macOS default PATH, which is
# what `tera up` relies on to find its sibling daemon.
PKG_ID=ai.teraflock.tera
INSTALL_ROOT=/usr/local/bin

[ "$(uname -s)" = Darwin ] || { log "ERROR: build-pkg.sh needs macOS (pkgbuild)"; exit 1; }
for b in "$flockd_bin" "$tera_bin"; do
  [ -f "$b" ] || { log "ERROR: missing binary $b"; exit 1; }
done

if ! have_apple_installer_secrets; then
  skip_or_fail pkg "installer secrets missing: $(missing "${APPLE_INSTALLER_SECRETS[@]}")"
fi
require_tool rcodesign "run scripts/release/install-tools.sh"
require_tool pkgbuild "Xcode command line tools"

# Refuse to wrap binaries that are not themselves signed: the notary would
# reject the package, and the check is free.
for b in "$flockd_bin" "$tera_bin"; do
  if ! codesign -dvv "$b" 2>&1 | grep -q 'Authority=Developer ID Application'; then
    log "ERROR: $(basename "$b") is not Developer ID signed; the pkg leg needs the macos leg"
    exit 1
  fi
done

umask 077
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
printf '%s' "$APPLE_INSTALLER_CERTIFICATE" | base64 -d >"$tmp/installer.p12"
printf '%s' "$APPLE_INSTALLER_CERTIFICATE_PASSWORD" >"$tmp/p12.pass"
printf '%s' "$APPLE_API_KEY" | base64 -d >"$tmp/AuthKey.p8"
rcodesign encode-app-store-connect-api-key -o "$tmp/asc.json" \
  "$APPLE_API_ISSUER" "$APPLE_API_KEY_ID" "$tmp/AuthKey.p8" >/dev/null 2>&1

root="$tmp/root$INSTALL_ROOT"
mkdir -p "$root"
install -m 0755 "$flockd_bin" "$root/flockd"
install -m 0755 "$tera_bin" "$root/tera"

log "pkg: building $(basename "$out") ($PKG_ID $version -> $INSTALL_ROOT)"
pkgbuild --root "$tmp/root" --identifier "$PKG_ID" --version "$version" \
  --install-location / "$tmp/unsigned.pkg" >/dev/null

log "pkg: signing (Developer ID Installer, timestamped)"
cp "$tmp/unsigned.pkg" "$tmp/signed.pkg"
rcodesign sign \
  --p12-file "$tmp/installer.p12" --p12-password-file "$tmp/p12.pass" \
  "$tmp/signed.pkg"
# The signing identity must be an Installer certificate; an Application
# one signs a xar happily and then fails at the notary, minutes later.
if ! pkgutil --check-signature "$tmp/signed.pkg" | grep -q 'Developer ID Installer'; then
  log "ERROR: package is not signed by a 'Developer ID Installer' certificate:"
  pkgutil --check-signature "$tmp/signed.pkg" >&2 || true
  exit 1
fi

# Same wait as sign-macos.sh and for the same reason: waiting is what makes
# "published = notarized" true.
log "pkg: submitting for notarization (waiting for Apple)"
rcodesign notary-submit --api-key-file "$tmp/asc.json" \
  --wait --max-wait-seconds "${NOTARY_MAX_WAIT_SECONDS:-3600}" \
  "$tmp/signed.pkg"

# Unlike the bare binaries, a package can carry its ticket: after this,
# Gatekeeper verifies it with no network at all.
log "pkg: stapling the notarization ticket"
rcodesign staple "$tmp/signed.pkg"
xcrun stapler validate "$tmp/signed.pkg" >/dev/null

mv "$tmp/signed.pkg" "$out"
log "pkg: $(basename "$out") signed, notarized and stapled"
