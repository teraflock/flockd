#!/usr/bin/env bash
# goreleaser post hook for every build target; acts only on .exe (flockd,
# tera for windows/amd64) and is a no-op for the rest. Authenticode via
# Azure Trusted Signing using jsign, so the whole release stays on one
# Linux runner (the azure/trusted-signing-action is Windows-only).
# Policy: scripts/release/lib.sh. teraflock/flockd#26, docs#37.
#
# Needs secrets AZURE_TENANT_ID, AZURE_CLIENT_ID, AZURE_CLIENT_SECRET (a
# service principal with the "Trusted Signing Certificate Profile Signer"
# role) and variables AZURE_SIGNING_ENDPOINT (e.g. eus.codesigning.azure.net),
# AZURE_SIGNING_ACCOUNT, AZURE_SIGNING_PROFILE. The client secret goes to
# the token endpoint through a temp file, the access token reaches jsign
# through the environment: neither is ever on a command line.
set -euo pipefail
# shellcheck source=lib.sh
. "$(dirname "$0")/lib.sh"

file=${1:?usage: sign-windows.sh <file>}
case "$file" in
*.exe | *.dll | *.msi) ;;
*) exit 0 ;;
esac
name=$(basename "$file")

if ! have_azure_secrets; then
  skip_or_fail windows "Azure Trusted Signing settings missing: $(missing "${AZURE_SECRETS[@]}" "${AZURE_VARS[@]}")"
fi
require_tool java "a JRE (ubuntu-latest runners have one)"
require_tool curl "curl"
require_tool jq "jq"
jar=${JSIGN_JAR:-}
if [ ! -f "$jar" ]; then
  log "ERROR: JSIGN_JAR is not set or missing (run scripts/release/install-tools.sh)"
  exit 1
fi

umask 077
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
printf '%s' "$AZURE_CLIENT_SECRET" >"$tmp/secret"
# OAuth2 client-credentials token scoped to the Trusted Signing service.
AZURE_SIGNING_TOKEN=$(curl -fsS --retry 3 -X POST \
  --data-urlencode grant_type=client_credentials \
  --data-urlencode "client_id=$AZURE_CLIENT_ID" \
  --data-urlencode scope=https://codesigning.azure.net/.default \
  --data-urlencode "client_secret@$tmp/secret" \
  "${AZURE_AUTHORITY_HOST:-https://login.microsoftonline.com}/$AZURE_TENANT_ID/oauth2/v2.0/token" |
  jq -r '.access_token // empty')
if [ -z "$AZURE_SIGNING_TOKEN" ]; then
  log "ERROR: windows: could not obtain an Azure access token for the signing service"
  exit 1
fi
export AZURE_SIGNING_TOKEN

log "windows: signing $name (Authenticode, Azure Trusted Signing, RFC 3161 timestamp)"
java -jar "$jar" \
  --storetype TRUSTEDSIGNING \
  --keystore "${AZURE_SIGNING_ENDPOINT#https://}" \
  --storepass env:AZURE_SIGNING_TOKEN \
  --alias "$AZURE_SIGNING_ACCOUNT/$AZURE_SIGNING_PROFILE" \
  --alg SHA-256 \
  --tsaurl http://timestamp.acs.microsoft.com --tsmode RFC3161 \
  "$file"
log "windows: $name signed"
