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
- **CLI/TUI** (`cmd/tera`): up/down/status/login/models(list|pin|rm)/
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
- **ModelAssignment handling**: DONE (plan 05, 2026-09-01) —
  `internal/assign` runs coordinator placements under the operator's
  consent: `[models] mesh_managed` (default on; live toggle via
  `PUT /api/v1/limits`, persisted in the limits overlay), `max_disk_mb`
  (a placement that cannot fit after evicting other *mesh-placed* models is
  declined), `pin`/`exclude`, and cache **origin** (`operator` vs `mesh`;
  the mesh can only evict what it placed — `models.Manager.EnsureOrigin`
  scopes LRU eviction to mesh-origin entries for mesh fetches). Downloads
  wait for AC power. Every step is reported as a `ModelState`
  (`assigned`/`downloading`/`ready`, one-shot `declined`/`failed`,
  `evicted`) and surfaces in `/api/v1/models` (`origin`, `assignment`)
  and the `model_assignment` SSE event. Mock-runtime nodes decline with a
  reason so the coordinator backs off.
- **ConfigUpdate** from coordinator: DONE — heartbeat cadence re-arms the
  running loop; `max_concurrent_requests` becomes a ceiling
  (min(operator budget, coordinator)) enforced at dispatch admission
  (`over-capacity` reject).
- **Enroll-without-restart**: DONE — `POST /api/v1/enroll` enrolls (or
  re-enrolls after a mesh-CA rotation, which the startup path cannot do)
  and swaps the tunnel client live. `tera login`'s claim-code file +
  restart flow still works for the CLI.
- **Cert rotation**: `enroll.RotateIfNeeded` scaffolded (re-enrolls within
  7 days of expiry); needs the real coordinator's rotation semantics and a
  call site on session start.

## TODO for the next developer (rough priority)

1. **Real llama-server E2E**: stand up the `runtimes/` build CI, publish an
   artifact manifest, and add an opt-in integration test
   (`FLOCKD_TEST_LLAMA_SERVER_PATH=… go test -tags realllama`).
2. **Phase 1 tunnel remainder**: QUIC `Dialer` (quic-go); cert rotation call
   site. (ModelAssignment and ConfigUpdate are done; enrollment against the
   real coordinator works — see below.)
3. **Limits persistence**: DONE — PUT /api/v1/limits persists to a
   daemon-owned `<data_dir>/limits.toml` overlay (config.toml untouched).
4. **Keychain**: move `node.key` and `local_api_token` to
   Keychain/DPAPI/secret-service (files are 0600 today, documented).
5. **Windows**: GetLastInputInfo idle source, GlobalMemoryStatusEx +
   WMI/NVML hardware, SCM via `x/sys/windows/svc`, job-object child
   management. Budget real time for this (SPEC §13.7).
6. **Governor extras**: foreground-GPU-usage signal, screen-lock signal
   (macOS `CGSession`, logind `LockedHint`).
7. **VRAM measurement on discrete GPUs**: memory admission uses the host
   footprint (correct on unified memory); on CUDA/ROCm boxes the estimate
   is kept because the child's VRAM use is not visible without NVML.
8. **SSE auth for EventSource**: browsers can't set headers on
   EventSource; support `?token=` query param (constant-time compare) or
   cookie for `/api/v1/events` so the React dash can use SSE instead of
   polling.
9. **Release eng**: wire real signing (cosign, notarytool) into
   `.goreleaser.yaml` — the config carries `TODO(keys)` markers; brew tap +
   winget publish once orgs/certs exist.
10. **OpenAPI spec**: DONE — `api/openapi.yaml` *generates* the management
    router and wire types (oapi-codegen, `make gen`, CI-guarded via
    `make gen-check`) plus the dashboard's and desktop app's TS types.
    Never edit `internal/localapi/gen/` by hand.

## Memory admission, idle unload and the `cached` state (plan 17 A/E)

The daemon now budgets memory and tells the coordinator the truth about
what is loaded versus merely on disk.

- **Budget**: `budget.max_ram_mb` (0 = auto: half of physical RAM on
  unified memory / CPU-only, `vram × max_vram_percent` on discrete GPUs).
  `internal/memory` owns the derivation, the pre-load estimate
  (`EstimateMB`: file × 1.15 + KV term + 256 MB, or catalog `min_ram_mb`
  if larger) and per-process measurement (`proc_pid_rusage`
  `ri_phys_footprint` on macOS via a cgo-free libSystem trampoline — the
  same mechanism x/sys/unix uses; `smaps_rollup` Pss on Linux; unsupported
  on Windows, estimate stays). The llama.cpp adapter fills
  `Stats.MemUsedMB` from it in `Health()`; modelops samples every 30 s and
  replaces estimates with measurements (except on discrete GPUs, where the
  host footprint misses VRAM — TODO(nvml)).
- **Admission** (`modelops.LoadInstanceOrigin`): download first (inside
  `max_disk_mb`), then, under `admitMu`, if `used + estimate > budget`
  unload idle instances — mesh-placed before operator-placed, LRU by last
  request within each group, never the default model, never one with
  requests in flight — and if still over, fail with
  `modelops.ErrOverMemory`. The artifact stays on disk.
- **Idle unload**: `models.idle_unload_s` (default 900, 0 = never, default
  model exempt); `modelops.RunHousekeeping` unloads instances idle past it.
  `ModelRow.idle_since` shows when an idle instance last served.
- **`cached` (the coordinator contract)**: `ModelState.state` is a free
  string, so no proto change. The node reports `cached` for any complete
  artifact on disk that is not loaded — mesh *and* operator origin, since
  both are legitimately serveable — in Hello/heartbeat model lists, and as
  a one-shot `ModelStateUpdate` when a placement could not be admitted for
  memory (`assign` maps `ErrOverMemory` → `cached`, never `declined`) and
  when a model is idle-unloaded (`modelops.OnUnloaded → assign.Unloaded`).
  `ready` in a heartbeat now strictly means loaded and serving. When the
  coordinator re-sends a `ModelAssignment` for a `cached` model the node
  loads it from disk (no `downloading` report) and reports `ready`; the
  usual `assigned → downloading → ready` sequence is unchanged for new
  models. The coordinator should count `cached` as a warm candidate that
  costs a load, not a live replica, and read `ram_used_mb` /
  `vram_used_mb` (now filled from measured footprints; equal on unified
  memory) for real headroom. A placement is never declined for memory
  alone; `max_disk_mb` declines (`does not fit in max_disk_mb`) still are.
- **Disk store**: `Manager.List()` stats files (`missing` state, dropped
  from the budget), `Stats()` sizes the store for `status.disk`,
  `GCPartials` (7 days) and `Retain` (`models.retention_days`) run on
  start and hourly, `Reconcile` adopts unindexed `<id>.gguf` files that
  match a catalog entry. `max_disk_mb`, `retention_days`, `max_ram_mb` and
  `idle_unload_seconds` are live via `PUT /api/v1/limits` and persist in
  `limits.toml`.
- **Activity feed** (`internal/activity`): a 200-row ring of what happened
  (download_started/downloaded/download_failed/loaded/unloaded/evicted/
  declined/missing/assignment/update_available with actor mesh|operator|
  daemon), `GET /api/v1/activity` newest first, SSE `activity`.
- **Update check** (`internal/update`): `update.feed_url` polled 30 s after
  start and hourly; semver compare against the build version (dev builds
  never nag); `status.update`, `POST /api/v1/update/check` (502 when the
  feed is unreachable — the background loop stays silent), SSE
  `update_available` once per discovered version, `tera status` line, TUI
  notice. No self-update.

## Mesh enrollment (Phase 1, working)

A real daemon now joins a real coordinator end to end:

```sh
tera login --claim-code <code>     # or the browser PKCE flow
FLOCKD_TUNNEL__COORDINATOR_ADDR=coordinator:9090 flockd
```

- `enroll.{Save,Read,Clear}ClaimCode` own the `<data_dir>/claim_code`
  handoff between the `tera` and `flockd` processes.
- `ensureEnrolled` (cmd/flockd) exchanges the code for credentials over a
  server-authenticated bootstrap dial, persists them, and clears the spent
  code. A failed enrollment keeps the code (an unreachable coordinator must
  not cost the operator their code).
- **The node ID is the coordinator-assigned one**, not the key fingerprint.
  This bit us on the first live mesh: the daemon announced its fingerprint
  and every session was rejected with "unknown node". `fakecoord` now
  enforces the same rule so the test suite catches it
  (`TestSessionRejectsUnenrolledNodeID`).
- `tera` resolves `data_dir` through the same config chain as `flockd`, so a
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

`tera token` prints the token (bare on stdout, hint on stderr, so
`| pbcopy` works). The CLI resolves it from `--token`, then `$TERA_TOKEN`,
then `<data_dir>/local_api_token`, and a 401 names the path it searched —
a data-dir mismatch between `tera` and `flockd` is the usual cause.

## Deviations from SPEC (deliberate, Phase 0)

- Tunnel transport is gRPC over H2/TLS behind the `Dialer` seam; QUIC is
  documented as the production transport but not implemented (SPEC allows
  the fallback; quic-go adds a lot of surface for zero Phase 0 value).
- `tera up` installs a **user-level** service (LaunchAgent / systemd
  --user), not a system daemon — no root, loopback only, simpler uninstall.
- Earnings numbers in standalone mode are explicitly labelled simulated
  (55 µcredits/completion token ≈ SPEC §7 "small" payout class).
