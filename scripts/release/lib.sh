# shellcheck shell=bash
# Shared policy for the release signing hooks (teraflock/flockd#26).
# Sourced by sign-macos.sh, sign-windows.sh, cosign-sign.sh and
# preflight.sh — never run directly.
#
# Three signing legs, each keyed on its own secrets/variables:
#   macos    Developer ID sign + notarize the universal flockd/tera
#            (rcodesign, from the Linux release job)
#   windows  Authenticode on flockd.exe/tera.exe via Azure Trusted Signing
#            (jsign, from the Linux release job)
#   cosign   Sigstore bundle over checksums.txt (goreleaser `signs:`)
#
# SIGNING_REQUIRED — a repository/org *variable*, default unset:
#   unset/""      a leg whose secrets are missing is skipped with a
#                 "unsigned release (SIGNING_REQUIRED not set)" line and
#                 the tag still publishes, unsigned for that leg (today's
#                 behaviour, so tags keep working until the keys exist).
#   true          every leg must have its secrets and must succeed;
#                 anything missing fails the job before publish.
#   macos,cosign  comma list: only the named legs are required.
#
# Two invariants that do not depend on SIGNING_REQUIRED:
#   * secrets that are present are always used (signing is never
#     silently skipped once it can happen), and
#   * a signing or notarization *failure* is always fatal, so no
#     release is ever half-signed or claims a signature it lacks.

set -u
set +x # never trace: the environment carries key material

log() { printf 'release: %s\n' "$*" >&2; }

# nonempty VAR... -> true when every named variable is set and non-empty.
nonempty() {
  local v
  for v in "$@"; do
    [ -n "${!v:-}" ] || return 1
  done
}

# missing VAR... -> the names (not values) of the unset/empty variables.
missing() {
  local v out=""
  for v in "$@"; do
    [ -n "${!v:-}" ] || out="$out $v"
  done
  printf '%s' "${out# }"
}

# Secrets (GitHub Actions org/repo secrets) and variables (`vars.*`) each
# leg needs. APPLE_TEAM_ID is intentionally not required here: rcodesign
# takes the team from the certificate; the desktop app's Tauri build is
# what needs it.
APPLE_SECRETS=(APPLE_CERTIFICATE APPLE_CERTIFICATE_PASSWORD APPLE_API_KEY_ID APPLE_API_ISSUER APPLE_API_KEY)
AZURE_SECRETS=(AZURE_TENANT_ID AZURE_CLIENT_ID AZURE_CLIENT_SECRET)
AZURE_VARS=(AZURE_SIGNING_ENDPOINT AZURE_SIGNING_ACCOUNT AZURE_SIGNING_PROFILE)

have_apple_secrets() { nonempty "${APPLE_SECRETS[@]}"; }
have_azure_secrets() { nonempty "${AZURE_SECRETS[@]}" "${AZURE_VARS[@]}"; }

# cosign_ready -> 0 when COSIGN_MODE selects a usable mode, 1 when unset,
# 2 when misconfigured (prints why on stderr).
cosign_ready() {
  case "${COSIGN_MODE:-}" in
  "") return 1 ;;
  keyless) return 0 ;;
  kms)
    if [ -z "${COSIGN_KMS_KEY:-}" ]; then
      log "COSIGN_MODE=kms needs COSIGN_KMS_KEY (a KMS URI such as awskms:///<key-arn>)"
      return 2
    fi
    return 0
    ;;
  *)
    log "unknown COSIGN_MODE=${COSIGN_MODE} (want keyless|kms|unset)"
    return 2
    ;;
  esac
}

# signing_required LEG -> true when SIGNING_REQUIRED covers LEG.
signing_required() {
  case ",${SIGNING_REQUIRED:-}," in
  ",true,") return 0 ;;
  *",$1,"*) return 0 ;;
  esac
  return 1
}

# skip_or_fail LEG REASON: exit 0 leaving the leg unsigned when that is
# allowed, exit 1 when SIGNING_REQUIRED covers the leg.
skip_or_fail() {
  if signing_required "$1"; then
    log "ERROR: $1 signing is required (SIGNING_REQUIRED=${SIGNING_REQUIRED:-}) but $2"
    exit 1
  fi
  log "unsigned release (SIGNING_REQUIRED not set): $1 leg skipped: $2"
  exit 0
}

# require_tool NAME HINT -> fails the job when NAME is not on PATH. Called
# only after a leg has decided to sign, so a laptop snapshot without the
# tools still works.
require_tool() {
  command -v "$1" >/dev/null 2>&1 && return 0
  log "ERROR: $1 not found on PATH ($2)"
  exit 1
}
