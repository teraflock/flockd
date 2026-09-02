# flockd architecture

`flockd` is a single static Go binary; `tera` (CLI/TUI) is its sibling.
Everything platform-specific is isolated behind build-tagged files or
well-marked stubs. llama.cpp runs as a **supervised subprocess**, never
cgo (SPEC §A1.3).

```
                          ┌────────────────────────────────────────────┐
                          │                   flockd                    │
                          │                                            │
   OpenAI SDKs ──HTTP──▶  │  localapi ───────┐                         │
   (localhost:7777/v1)    │   /v1 (OpenAI)   │                         │
                          │   /api/v1 (mgmt) │        ┌─────────────┐  │
   tera CLI/TUI ──────▶   │   / (web dash,   ├──▶ engine ──▶ runtime │  │
   web dashboard          │      go:embed)   │   │  ▲      │ (iface) │  │
                          │                  │   │  │      └──┬──────┘  │
                          │                  │  governor      │ subprocess
                          │  tunnel ─────────┘   │  ▲      llama-server │
                          │   │ ▲ dispatches     │  │      (or mock)    │
                          │   │ │ heartbeats   idle/power                │
                          │   │ │              sources                   │
                          └───┼─┼────────────────────────────────────── ┘
                              │ │ gRPC (H2/TLS today, QUIC planned)
                              ▼ │
                    coordinator (Phase 1) or in-process
                    fake coordinator (--standalone, Phase 0)
```

## Package map

| package | role |
|---|---|
| `internal/config` | koanf config: defaults ← TOML ← `FLOCKD_*` env. Every knob in [config.md](config.md). |
| `internal/hardware` | CapabilityProfile detection. Real on macOS (`system_profiler`/`sysctl`), nvidia-smi on Linux, stub on Windows. |
| `internal/governor` | The make-or-break piece. Polls `IdleSource`/`PowerSource`, applies `serve: always\|idle-only\|scheduled`, battery/thermal guards, and **instant-yield**: on activity, in-flight requests get `yield_grace` (2s) to drain, then are cancelled; the node reports YIELDED. Heavily tested with a fake clock and fake signal sources. |
| `internal/runtime` | The `Runtime`/`Instance` interfaces (SPEC §A1.3 verbatim) + deterministic mock. |
| `internal/runtime/llamacpp` | Artifact fetcher (pinned manifest, SHA256-verify), supervisor (health-gate, crash-restart with backoff), and the HTTP/SSE translation to llama-server's OpenAI API on an ephemeral loopback port. |
| `internal/models` | Catalog client (teraflock/models YAML/JSON), resumable GGUF downloads (Range), SHA256 verification (refuses mismatches), pin/exclude, LRU eviction under `max_disk_mb`; each cache entry carries an origin (`operator`/`mesh`) so mesh-triggered eviction never touches the operator's own models. |
| `internal/assign` | Coordinator placement executor (plan 05): applies `ModelAssignment` pushes under the operator's consent (`mesh_managed`, budget, pin/exclude), waits for AC power, reports `assigned`/`downloading`/`ready`/`declined`/`failed`/`evicted` over the tunnel and as `model_assignment` SSE events. |
| `internal/engine` | Single serving funnel: model lookup → governor admission → runtime → telemetry metering. Shared by localapi and tunnel so both paths behave identically. |
| `internal/tunnel` | Node side of `flock.tunnel.v1.TunnelService`: Hello/HelloAck, heartbeat loop, Dispatch (signature-verified against the pinned coordinator Ed25519 key), TokenChunk streaming, Cancel, Challenge (fingerprint probes), Drain, jittered-backoff reconnect. Transport is behind a `Dialer` interface — gRPC/H2 now, QUIC (quic-go) is the planned production transport. |
| `internal/tunnel/fakecoord` | In-process fake coordinator over bufconn implementing the same proto service: Enroll (real CSR signing with a throwaway CA), Session, and a driver API to push dispatches/challenges. Powers `--standalone` and the tunnel test-suite. |
| `internal/enroll` | Ed25519 identity (0600, never leaves the device), CSR, Enroll RPC, credential storage, mTLS client config, PKCE-style loopback login flow, cert-rotation scaffold. |
| `internal/localapi` | Loopback HTTP: OpenAI-compatible `/v1/*`, management `/api/v1/*` (bearer token), SSE `/api/v1/events`, embedded web dashboard. |
| `internal/telemetry` | Rolling tok/s window, request counters, heartbeat assembly. |
| `internal/svc` | launchd (macOS, real), systemd user unit (Linux, real), Windows SCM (stub with instructions). |
| `web/` | Vite+React+TS+Tailwind dashboard, TanStack Query polling `/api/v1`; `dist/` committed and embedded via `go:embed`. |

## Key seams

- **`runtime.Runtime`** — llamacpp and mock implement the same three methods.
- **`tunnel.Dialer`** — swap gRPC/H2 for QUIC without touching session
  logic.
- **`engine.Engine`** — the local API and coordinator dispatch share one
  code path, so governor yields, metering and model routing can't drift
  apart.
- **fakecoord** — the daemon is fully exercisable with zero control-plane
  infrastructure; the control-plane team codes against the same proto.

## Request flow (standalone chat completion)

1. SDK POSTs `/v1/chat/completions` with `stream: true`.
2. localapi builds a `runtime.CompletionRequest` (seed always set).
3. engine: governor `Admit` (503 if yielded/paused) → instance.
4. mock or llama-server generates; chunks flow back as SSE
   `chat.completion.chunk` frames, final chunk carries usage, then
   `data: [DONE]`.
5. telemetry records tokens (tok/s ticker) and the simulated payout that
   the earnings endpoints report.

The same request arriving as a coordinator `DispatchRequest` is
signature-verified, acked, run through the identical engine path, and
streamed back as `TokenChunk`s.
