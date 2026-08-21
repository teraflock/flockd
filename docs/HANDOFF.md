# HANDOFF — implemented vs stubbed vs TODO

Status as of Phase 0 completion. `go build ./... && go vet ./... && go
test ./...` are green; `scripts/smoke.sh` proves the end-to-end standalone
path.

## Fully implemented and tested

- **Config** (`internal/config`): koanf defaults ← TOML ← `FLOCKD_*` env;
  validation; every knob documented in `docs/config.md`.
- **Runtime interface** (`internal/runtime`): exactly SPEC §A1.3.
  - **mock runtime**: deterministic by seed, configurable tok/s,
    embeddings, cancellation; used by tests, fakecoord path and
    `--standalone --runtime=mock`.
  - **llamacpp adapter**: artifact manifest fetch + SHA256-verified
    download of a pinned `llama-server`; supervisor with health-gating and
    crash-restart w/ backoff (tested via re-exec helper process); ephemeral
    loopback port; OpenAI-HTTP/SSE translation (tested against a fake
    llama-server). *Not yet run against a real llama-server binary in CI —
    no artifact CDN exists; set `runtime.llama_server_path` to a local
    build to use it today.*
- **Governor** (`internal/governor`): idle-only/always/scheduled policies,
  battery + thermal guards, schedule windows w/ midnight wrap, instant
  yield (drain-or-cancel within `yield_grace`), admission API, live policy
  swap, race-clean. 16-test suite incl. fake-clock yield-latency proofs and
  a real-clock <1s end-to-end latency bound. Darwin idle (`ioreg`
  HIDIdleTime) and battery (`pmset`) parsers are real w/ fixture tests.
- **Models manager** (`internal/models`): YAML/JSON catalog, resumable
  downloads (Range/206), SHA256 verify with refusal + poisoned-partial
  cleanup, LRU eviction under budget, pin survival, state persistence.
- **Tunnel** (`internal/tunnel`): full session client (Hello/HelloAck,
  heartbeats, Dispatch with Ed25519 signature verification, TokenChunk
  streaming, EmbeddingResult, Cancel mid-stream, Challenge with output
  sha256, Drain, Goodbye) + jittered-backoff reconnect. Tested end-to-end
  over bufconn incl. corrupted-signature rejection and reconnect-after-kick.
- **Fake coordinator** (`internal/tunnel/fakecoord`): real gRPC service
  over bufconn, Enroll with actual CA/CSR signing, driver API for tests,
  session kick. Runs inside `flockd --standalone`.
- **Enrollment** (`internal/enroll`): Ed25519 identity (0600), CSR, Enroll
  RPC + credential persistence + pinned coordinator key, mTLS client
  config, PKCE-style loopback login flow (state + S256 challenge) with
  tests.
- **Local API** (`internal/localapi`): `/v1/chat/completions` (SSE +
  non-stream), `/v1/completions`, `/v1/embeddings`, `/v1/models`;
  `/api/v1/{status,models,earnings,limits,logs}` + SSE `/api/v1/events`;
  bearer-token auth (per-install token file, 0600); loopback-bind warning;
  OpenAI error shapes; 503 on yield. 12-test suite.
- **Telemetry**: rolling 1-minute tok/s window, counters, heartbeat
  assembly.
- **CLI/TUI** (`cmd/flock`): up/down/status/login/models(list|pin|rm)/
  limits/earnings/redeem/dashboard(TUI + --web)/version/uninstall(--purge).
  Bubbletea dashboard with sparkline, earnings ticker, model slots.
- **Web dashboard** (`web/`): React+TS+Tailwind(4)+TanStack Query source,
  built successfully; `dist/` committed and embedded via `go:embed`.
- **svc**: launchd LaunchAgent (real, plist render tested) and systemd
  user unit (real) management.
- **Daemon** (`cmd/flockd`): full wiring, graceful shutdown, structured
  logging with in-memory ring for `/api/v1/logs`.

## Stubbed (compiles, documented, returns useful errors)

- **Windows**: hardware detection (CPU env var only), idle source (assume
  idle w/ one-time warning), power source (reports AC), SCM service
  manager (`ErrUnsupported` + manual `sc.exe` instructions), process
  terminate (Kill, no console event).
- **Linux idle**: logind DBus not wired; assumes idle (correct default for
  headless boxes, warned once). Battery/thermal via sysfs are real.
- **Reputation panel** in the TUI: placeholder until the trust engine
  exists (Phase 3).
- **ModelAssignment handling**: dispatch plumbing exists; the handler
  currently logs (download+load+evict loop needs the llamacpp runtime on a
  real node — wire `models.Manager` + `engine.Register` in
  `cmd/flockd/startTunnel`).
- **ConfigUpdate** from coordinator: logged, not applied mid-session.
- **Enroll-without-restart**: `flock login` stores the claim code and the
  daemon consumes it on next start (`flock down && flock up`). Triggering
  enrollment through the running daemon's local API is still TODO.
- **Cert rotation**: `enroll.RotateIfNeeded` scaffolded (re-enrolls within
  7 days of expiry); needs the real coordinator's rotation semantics and a
  call site on session start.

## TODO for the next developer (rough priority)

1. **Real llama-server E2E**: stand up the `runtimes/` build CI, publish an
   artifact manifest, and add an opt-in integration test
   (`FLOCKD_TEST_LLAMA_SERVER_PATH=… go test -tags realllama`).
2. **Phase 1 tunnel remainder**: QUIC `Dialer` (quic-go); cert rotation call
   site; ModelAssignment → download/load/evict. (Enrollment against the real
   coordinator now works — see below.)
3. **Limits persistence**: PUT /api/v1/limits should write back to
   config.toml (koanf marshal) — currently live-only.
4. **Keychain**: move `node.key` and `local_api_token` to
   Keychain/DPAPI/secret-service (files are 0600 today, documented).
5. **Windows**: GetLastInputInfo idle source, GlobalMemoryStatusEx +
   WMI/NVML hardware, SCM via `x/sys/windows/svc`, job-object child
   management. Budget real time for this (SPEC §13.7).
6. **Governor extras**: foreground-GPU-usage signal, screen-lock signal
   (macOS `CGSession`, logind `LockedHint`).
7. **VRAM budget enforcement**: `budget.max_vram_percent` is passed to the
   runtime but llama-server flag mapping (`--n-gpu-layers` heuristics) is
   not implemented.
8. **SSE auth for EventSource**: browsers can't set headers on
   EventSource; support `?token=` query param (constant-time compare) or
   cookie for `/api/v1/events` so the React dash can use SSE instead of
   polling.
9. **Release eng**: wire real signing (cosign, notarytool) into
   `.goreleaser.yaml` — the config carries `TODO(keys)` markers; brew tap +
   winget publish once orgs/certs exist.
10. **OpenAPI spec** for the local API (SPEC §A5.4) and generated TS types
    for the dashboard.

## Mesh enrollment (Phase 1, working)

A real daemon now joins a real coordinator end to end:

```sh
flock login --claim-code <code>     # or the browser PKCE flow
FLOCKD_TUNNEL__COORDINATOR_ADDR=coordinator:9090 flockd
```

- `enroll.{Save,Read,Clear}ClaimCode` own the `<data_dir>/claim_code`
  handoff between the `flock` and `flockd` processes.
- `ensureEnrolled` (cmd/flockd) exchanges the code for credentials over a
  server-authenticated bootstrap dial, persists them, and clears the spent
  code. A failed enrollment keeps the code (an unreachable coordinator must
  not cost the operator their code).
- **The node ID is the coordinator-assigned one**, not the key fingerprint.
  This bit us on the first live mesh: the daemon announced its fingerprint
  and every session was rejected with "unknown node". `fakecoord` now
  enforces the same rule so the test suite catches it
  (`TestSessionRejectsUnenrolledNodeID`).
- `flock` resolves `data_dir` through the same config chain as `flockd`, so a
  moved data dir cannot silently break the handoff.
- `--standalone` enrolls into `<data_dir>/standalone/` so it can never
  overwrite a real mesh identity.
- `tunnel.insecure = true` (config or `FLOCKD_TUNNEL__INSECURE`) dials the
  coordinator over plaintext gRPC — needed against the dev coordinator until
  mTLS termination lands, never for real deployments.
- The daemon reports `runtime_build_id` in its CapabilityProfile
  (`mock-runtime-v1`, or the pinned llama.cpp build id). The coordinator
  needs it to pick fingerprint challenges calibrated for that runtime;
  without it a node cannot be fingerprint-verified at all.

## Local models and real llama.cpp

`artifact_url` accepts `file://` paths (and bare absolute paths), so a node
serves an existing GGUF collection **in place** — no download, no copy, and
the LRU never evicts a file the daemon does not own (`internal/models/
local.go`). A real 64-hex `sha256` is verified before serving; `TODO-verify`
or empty serves with a loud unverified warning.

Verified against a real `llama-server` for the first time (Homebrew
llama.cpp b10360, M3 Max/Metal): Qwen3.5-9B Q4_K_M ~50 tok/s, a 20GB Q4_K_M
~64 tok/s, SHA verification 2.3s and 11s respectively.

**Reasoning models**: the adapter passes `--reasoning-format none` so
chain-of-thought stays inline in `content`. llama.cpp otherwise routes it to
`reasoning_content` and leaves `content` empty, so a generation truncated
mid-thought returned *nothing* while still billing the customer for the
tokens — and canary comparison, which diffs output strings, would have seen
empty output for every reasoning model.

## Local API auth

Bearer parsing tolerates surrounding whitespace and a case-insensitive
scheme: the token is pasted by hand out of a file or terminal, and a stray
leading space produced an "invalid token" indistinguishable from a wrong
one. `/api/v1/events` additionally accepts `?token=` because EventSource
cannot set headers; every other route rejects query tokens.

`flock token` prints the token (bare on stdout, hint on stderr, so
`| pbcopy` works). The CLI resolves it from `--token`, then `$FLOCK_TOKEN`,
then `<data_dir>/local_api_token`, and a 401 names the path it searched —
a data-dir mismatch between `flock` and `flockd` is the usual cause.

## Deviations from SPEC (deliberate, Phase 0)

- Tunnel transport is gRPC over H2/TLS behind the `Dialer` seam; QUIC is
  documented as the production transport but not implemented (SPEC allows
  the fallback; quic-go adds a lot of surface for zero Phase 0 value).
- `flock up` installs a **user-level** service (LaunchAgent / systemd
  --user), not a system daemon — no root, loopback only, simpler uninstall.
- Earnings numbers in standalone mode are explicitly labelled simulated
  (55 µcredits/completion token ≈ SPEC §7 "small" payout class).
