#!/usr/bin/env bash
# Install the release signing tools the goreleaser hooks call, pinned by
# version and sha256 (teraflock/flockd#26):
#   rcodesign  Apple code signing + notarization from Linux/macOS
#              (indygreg/apple-platform-rs)
#   jsign      Authenticode via Azure Trusted Signing (ebourg/jsign; needs
#              a JRE, which ubuntu-latest and macos-latest runners carry)
# cosign is installed by sigstore/cosign-installer in the workflow.
#
# Idempotent. Installs under $RELEASE_TOOLS_DIR (default
# ~/.cache/teraflock-release-tools); in GitHub Actions it also appends
# the bin dir to GITHUB_PATH and exports JSIGN_JAR via GITHUB_ENV.
#
# Bumping a pin: download the new asset, `shasum -a 256` it, and update
# both lines together. Never paste a hash you did not compute.
set -euo pipefail

RCODESIGN_VERSION=0.29.0
RCODESIGN_SHA256_LINUX_X86_64=dbe85cedd8ee4217b64e9a0e4c2aef92ab8bcaaa41f20bde99781ff02e600002
RCODESIGN_SHA256_MACOS_UNIVERSAL=d98372d5524226ccf9dc0eda03d4e4f5826182dabb2fc3f2bd303ed9113a748d
JSIGN_VERSION=7.5
JSIGN_SHA256=602a51c3545a6dc4fb99bd2ea7152b26d1345916d0c93ddfbd5936cb735af91c

dir="${RELEASE_TOOLS_DIR:-$HOME/.cache/teraflock-release-tools}"
bin="$dir/bin"
mkdir -p "$bin"

case "$(uname -s)/$(uname -m)" in
linux/x86_64 | Linux/x86_64)
  rc_asset="apple-codesign-${RCODESIGN_VERSION}-x86_64-unknown-linux-musl"
  rc_sha="$RCODESIGN_SHA256_LINUX_X86_64"
  ;;
Darwin/*)
  rc_asset="apple-codesign-${RCODESIGN_VERSION}-macos-universal"
  rc_sha="$RCODESIGN_SHA256_MACOS_UNIVERSAL"
  ;;
*)
  echo "install-tools: no rcodesign pin for $(uname -s)/$(uname -m)" >&2
  exit 1
  ;;
esac

verify() { # verify FILE SHA256
  echo "$2  $1" | (sha256sum -c --quiet - 2>/dev/null || shasum -a 256 -c --quiet -)
}

fetch() { # fetch URL DEST SHA256
  if [ -f "$2" ] && verify "$2" "$3"; then
    return 0
  fi
  echo "install-tools: downloading $(basename "$2")" >&2
  curl -fsSL --retry 3 -o "$2.tmp" "$1"
  verify "$2.tmp" "$3" || {
    echo "install-tools: sha256 mismatch for $1" >&2
    rm -f "$2.tmp"
    exit 1
  }
  mv "$2.tmp" "$2"
}

if [ ! -x "$bin/rcodesign" ] || ! "$bin/rcodesign" --version 2>/dev/null | grep -q "$RCODESIGN_VERSION"; then
  fetch "https://github.com/indygreg/apple-platform-rs/releases/download/apple-codesign%2F${RCODESIGN_VERSION}/${rc_asset}.tar.gz" \
    "$dir/${rc_asset}.tar.gz" "$rc_sha"
  tar -xzf "$dir/${rc_asset}.tar.gz" -C "$dir" "${rc_asset}/rcodesign"
  mv "$dir/${rc_asset}/rcodesign" "$bin/rcodesign"
  chmod +x "$bin/rcodesign"
fi

fetch "https://github.com/ebourg/jsign/releases/download/${JSIGN_VERSION}/jsign-${JSIGN_VERSION}.jar" \
  "$dir/jsign-${JSIGN_VERSION}.jar" "$JSIGN_SHA256"

echo "install-tools: rcodesign $("$bin/rcodesign" --version | awk '{print $NF}') and jsign ${JSIGN_VERSION} in $dir" >&2
if [ -n "${GITHUB_PATH:-}" ]; then
  echo "$bin" >>"$GITHUB_PATH"
fi
if [ -n "${GITHUB_ENV:-}" ]; then
  echo "JSIGN_JAR=$dir/jsign-${JSIGN_VERSION}.jar" >>"$GITHUB_ENV"
fi
echo "$dir"
