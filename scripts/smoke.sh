#!/usr/bin/env bash
# Phase 0 smoke test: build hived, run it standalone with the mock runtime,
# and prove the OpenAI-compatible endpoint + management API work end to end.
set -euo pipefail

cd "$(dirname "$0")/.."

PORT="${SMOKE_PORT:-7791}"
DATA_DIR="$(mktemp -d)"
BIN="$(mktemp -d)/hived"
LOG="$DATA_DIR/hived.log"

cleanup() {
  kill "$DAEMON_PID" 2>/dev/null || true
  wait "$DAEMON_PID" 2>/dev/null || true
  rm -rf "$DATA_DIR"
}
trap cleanup EXIT

echo "==> building hived"
go build -o "$BIN" ./cmd/hived

echo "==> starting hived --standalone --runtime=mock on :$PORT"
HIVED_GOVERNOR__SERVE_POLICY=always \
  "$BIN" --standalone --runtime=mock --data-dir "$DATA_DIR" --listen "127.0.0.1:$PORT" >"$LOG" 2>&1 &
DAEMON_PID=$!

for i in $(seq 1 50); do
  if curl -sf "http://127.0.0.1:$PORT/v1/models" >/dev/null 2>&1; then break; fi
  if ! kill -0 "$DAEMON_PID" 2>/dev/null; then
    echo "daemon died:"; cat "$LOG"; exit 1
  fi
  sleep 0.2
done

fail() { echo "FAIL: $1"; echo "--- daemon log ---"; cat "$LOG"; exit 1; }

echo "==> GET /v1/models"
curl -sf "http://127.0.0.1:$PORT/v1/models" | grep -q '"mock-8b-instruct"' || fail "/v1/models missing model"

echo "==> POST /v1/chat/completions (non-streaming)"
RESP=$(curl -sf -X POST "http://127.0.0.1:$PORT/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -d '{"model":"mock-8b-instruct","messages":[{"role":"user","content":"hello mesh"}],"max_tokens":16,"seed":7}')
echo "$RESP" | grep -q '"chat.completion"' || fail "non-streaming chat response malformed: $RESP"
echo "$RESP" | grep -q '"completion_tokens"' || fail "usage missing: $RESP"

echo "==> POST /v1/chat/completions (streaming SSE)"
STREAM=$(curl -sfN -X POST "http://127.0.0.1:$PORT/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -d '{"messages":[{"role":"user","content":"stream please"}],"max_tokens":8,"stream":true,"seed":7}')
echo "$STREAM" | grep -q 'chat.completion.chunk' || fail "no stream chunks"
echo "$STREAM" | grep -q 'data: \[DONE\]' || fail "stream missing [DONE]"

echo "==> POST /v1/completions"
curl -sf -X POST "http://127.0.0.1:$PORT/v1/completions" \
  -H 'Content-Type: application/json' \
  -d '{"prompt":"Once upon","max_tokens":8,"seed":3}' | grep -q '"text_completion"' || fail "/v1/completions"

echo "==> POST /v1/embeddings"
curl -sf -X POST "http://127.0.0.1:$PORT/v1/embeddings" \
  -H 'Content-Type: application/json' \
  -d '{"input":["alpha","beta"]}' | grep -q '"embedding"' || fail "/v1/embeddings"

TOKEN=$(cat "$DATA_DIR/local_api_token")

echo "==> /api/v1 auth enforcement"
CODE=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/api/v1/status")
[ "$CODE" = "401" ] || fail "expected 401 without token, got $CODE"

echo "==> GET /api/v1/status + earnings + models + limits + logs (authed)"
curl -sf -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:$PORT/api/v1/status" | grep -q '"state"' || fail "status"
curl -sf -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:$PORT/api/v1/earnings" | grep -q 'simulated' || fail "earnings"
curl -sf -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:$PORT/api/v1/models" | grep -q 'mock-8b-instruct' || fail "models"
curl -sf -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:$PORT/api/v1/limits" | grep -q 'serve_policy' || fail "limits"
curl -sf -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:$PORT/api/v1/logs?n=5" | grep -q 'logs' || fail "logs"

echo "==> GET /api/v1/events (SSE, first event)"
EVENTS=$(curl -sN --max-time 3 -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:$PORT/api/v1/events" || true)
echo "$EVENTS" | grep -q 'event: status' || fail "SSE events"

echo "==> GET / (embedded web dashboard)"
curl -sf "http://127.0.0.1:$PORT/" | grep -qi '<title>HiveGrid' || fail "web dashboard"

echo
echo "SMOKE OK — hived serves an OpenAI-compatible endpoint at http://127.0.0.1:$PORT/v1"
