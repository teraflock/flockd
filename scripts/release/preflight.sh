#!/usr/bin/env bash
# Release signing preflight (teraflock/flockd#26): decides, before anything
# is built or published, which legs will sign, and fails fast when
# SIGNING_REQUIRED covers a leg whose secrets or variables are missing.
# Prints a plan; in GitHub Actions also writes the step summary and
# exports to GITHUB_ENV:
#   TERAFLOCK_MACOS_SIGNED=true|false   read by .goreleaser.yaml templates
#                                       (cask quarantine hook, release footer)
#   TERAFLOCK_WINDOWS_SIGNED=true|false
#   TERAFLOCK_MACOS_PKG=true|false      the pkg job will build, sign,
#                                       notarize and staple the installer
#                                       and publish the cask; goreleaser
#                                       then skips its binary cask
#   GORELEASER_SIGN_ARGS="--skip=sign"  when COSIGN_MODE is unset
# "true" here means "this leg WILL sign, and its failure fails the job",
# which is what makes the template conditions safe: a published release
# with TERAFLOCK_MACOS_SIGNED=true was signed and notarized.
set -euo pipefail
# shellcheck source=lib.sh
. "$(dirname "$0")/lib.sh"

fail=0
macos=false pkg=false windows=false cosign=false sign_args=""

if have_apple_secrets; then
  macos=true
elif signing_required macos; then
  log "ERROR: macos leg required but Apple secrets missing: $(missing "${APPLE_SECRETS[@]}")"
  fail=1
fi

# The installer leg presupposes the macos leg: a pkg wrapping unsigned
# binaries would notarize-fail anyway, and publishing it would hand every
# brew user the exact quarantine problem the pkg exists to remove (#47).
if have_apple_installer_secrets && [ "$macos" = true ]; then
  pkg=true
elif signing_required pkg; then
  log "ERROR: pkg leg required but installer secrets missing: $(missing "${APPLE_INSTALLER_SECRETS[@]}") (or macos leg unsigned)"
  fail=1
fi

if have_azure_secrets; then
  windows=true
elif signing_required windows; then
  log "ERROR: windows leg required but Azure settings missing: $(missing "${AZURE_SECRETS[@]}" "${AZURE_VARS[@]}")"
  fail=1
fi

set +e
cosign_ready
ready=$?
set -e
case $ready in
0) cosign=true ;;
1)
  sign_args="--skip=sign"
  if signing_required cosign; then
    log "ERROR: cosign leg required but COSIGN_MODE is unset (docs#34)"
    fail=1
  fi
  ;;
2) fail=1 ;;
esac

state() { if [ "$1" = true ]; then echo "sign"; else echo "unsigned"; fi; }
plan="signing plan: macos=$(state $macos) pkg=$(state $pkg) windows=$(state $windows) cosign=$(state $cosign)${COSIGN_MODE:+ (COSIGN_MODE=$COSIGN_MODE)} SIGNING_REQUIRED=${SIGNING_REQUIRED:-unset}"
log "$plan"
if [ "$macos$pkg$windows$cosign" = "falsefalsefalsefalse" ] && [ -z "${SIGNING_REQUIRED:-}" ]; then
  log "unsigned release (SIGNING_REQUIRED not set): no signing secrets configured"
fi

if [ -n "${GITHUB_ENV:-}" ]; then
  {
    echo "TERAFLOCK_MACOS_SIGNED=$macos"
    echo "TERAFLOCK_MACOS_PKG=$pkg"
    echo "TERAFLOCK_WINDOWS_SIGNED=$windows"
    echo "GORELEASER_SIGN_ARGS=$sign_args"
  } >>"$GITHUB_ENV"
fi
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  {
    echo "### Release signing"
    echo
    echo "| leg | state |"
    echo "|---|---|"
    echo "| macOS Developer ID + notarization | $(state $macos) |"
    echo "| macOS installer .pkg (Developer ID Installer, notarized, stapled) | $(state $pkg) |"
    echo "| Windows Authenticode (Azure Trusted Signing) | $(state $windows) |"
    echo "| cosign bundle over checksums.txt | $(state $cosign)${COSIGN_MODE:+ ($COSIGN_MODE)} |"
    echo
    echo "SIGNING_REQUIRED=\`${SIGNING_REQUIRED:-unset}\`"
  } >>"$GITHUB_STEP_SUMMARY"
fi
exit $fail
