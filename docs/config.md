# flockd configuration

`flockd` resolves configuration in this order (later wins):

1. built-in defaults
2. TOML file — `--config <path>`, or `<data_dir>/config.toml` if it exists
3. environment variables — prefix `FLOCKD_`, `__` (double underscore) as the
   section separator: `FLOCKD_LOCAL_API__LISTEN=127.0.0.1:8080` sets
   `local_api.listen`
4. a handful of CLI flags (`--standalone`, `--runtime`, `--listen`,
   `--data-dir`, `--log-level`, `--log-file`) override everything for convenience

Every knob, with defaults:

```toml
# Where keys, certs, tokens, models and runtimes live. 0700.
data_dir = "~/.teraflock"

[log]
level  = "info"   # debug | info | warn | error
format = "text"   # text | json
file   = ""       # also append every record to this file (never rotated).
                  # The Windows logon task sets it (--log-file) because a
                  # task has no console; launchd/systemd capture stderr.

[local_api]
# Loopback only by default. Changing this exposes the management API and
# the OpenAI endpoints to your network — the bearer token is then your only
# protection. Homelab users only; you have been warned.
listen = "127.0.0.1:7777"
# Require the bearer token on /v1 too (management /api/v1 always requires
# it). Off by default so OPENAI_BASE_URL works with any placeholder key.
require_auth_v1 = false

[runtime]
kind = "llamacpp"            # llamacpp | mock
# Use an existing llama-server binary instead of downloading one.
llama_server_path = ""
# JSON manifest of pinned, SHA256-verified llama-server builds, published
# by teraflock/runtimes (manifests/schema.json is the source of truth).
# Defaults to the hosted manifest; llama_server_path overrides fetching.
artifact_manifest_url = "https://teraflock-downloads.s3.amazonaws.com/runtimes/llamacpp/manifest.json"
# Public key (ECDSA P-256, cosign.pub format) that runtime manifests and
# tarballs are cosign-signed with: an inline "-----BEGIN PUBLIC KEY-----"
# PEM or the path to a PEM file. Overrides the key built into the daemon;
# only for self-hosted coordinators / development catalogs signed with
# their own key. Never taken from the manifest or the artifact host.
artifact_signing_key = ""
# Refuse runtime manifests and artifacts that carry no cosign signature
# (and refuse to run without a pinned key). On by default: the stable
# manifest and every build it lists are signed with the key the daemon
# embeds. A runtime cached before signing existed is refused at the next
# start and re-fetched. Set false only for a self-hosted catalog that does
# not sign its builds.
require_signature = true
# Synthetic generation speed for kind=mock (tests, demos).
mock_tokens_per_sec = 120
# Context and slots are planned per load from the memory budget: after
# room is made for the smallest acceptable layout, spare memory is spent
# on context (up to max_context per request) across up to
# budget.max_concurrent slots, split evenly; slots are given up before a
# request gets less than min_context. The daemon logs the plan
# ("context plan": slots, ctx_per_slot, estimate_mb) at every load.
#
# Decision models (catalog `decision: true`, served by /v1/systemone) are
# not planned that way: a decision is one short forward pass and the model
# usually shares the node with a chat model, so they get a fixed, modest
# layout instead of the free budget — encoders (Laya, Julia: no KV cache,
# memory is one batch buffer sized by the context) 4 slots of up to 2048
# tokens, causal ones (Kev, Clef) 2 slots of up to 8192. A longer prompt
# is a 422. context_length below raises (pins) it for every model.
#
# Pin per-request context exactly (0 = planned). Slots still adapt.
context_length = 0
# Cap on per-request context, in tokens (0 = the model's training window).
# The KV cache is sized by slots × context, at the per-token cost read
# from the model's GGUF header (layers × KV heads × head size × 2 × 2 B):
# Llama-3.2 3B is 112 KB/token, Llama-3.1 8B 128 KB, Qwen3 8B 144 KB, so
# 16 slots at 16384 tokens is ~29 GB of KV for the 3B. Sizing follows the
# budget, not the other way round: a smaller budget plans fewer slots.
max_context = 16384
# Floor on per-request context; fewer slots before less context.
min_context = 8192

[governor]
serve_policy     = "idle-only"  # always | idle-only | scheduled
idle_after       = "2m"         # input quiet time before the node counts as idle
                                # (macOS: HID idle time; Linux: logind IdleHint of
                                # your seat session, so a desktop without an idle
                                # daemon never counts as idle — use always/scheduled;
                                # Windows: GetLastInputInfo in the daemon's session;
                                # headless/no logind: assumed idle, logged once).
                                # A locked screen counts as idle at once, whatever
                                # idle_after says (macOS: IOConsoleLocked via ioreg;
                                # Linux: logind LockedHint — any locker that tells
                                # logind, incl. a bare X session; Windows: not yet)
yield_grace      = "2s"         # drain-or-cancel window on operator activity
poll_interval    = "2s"         # how often idle/power signals are sampled
serve_on_battery = false        # never serve on battery by default
max_temp_celsius = 90.0         # pause above this temperature (0 disables;
                                # Linux thermal zones only — macOS and Windows
                                # report no temperature, so this never trips there)
schedule         = []           # serve_policy=scheduled windows, e.g. ["22:00-08:00"]

[budget]
max_vram_percent = 80   # ceiling for the runtime (passed to llama-server)
max_ram_mb       = 0    # memory budget for LOADED models. 0 = auto: half of
                        # physical RAM on unified-memory machines (Apple
                        # Silicon, CPU-only boxes), vram × max_vram_percent on
                        # discrete GPUs. A load that would exceed it first
                        # unloads idle models (mesh-placed before yours, least
                        # recently used first, never the default model, never
                        # one serving a request); if that is not enough the
                        # load is refused — mesh placements then stay on disk
                        # as `cached`. Live via PUT /api/v1/limits max_ram_mb.
max_concurrent   = 0    # slot ceiling per model and the node's dispatch cap;
                        # 0 = auto by accelerator class (16 with a GPU or
                        # unified memory, 2 CPU-only). Memory decides the
                        # actual slots per load; see [runtime] max_context.

[models]
manifest_path = ""              # local catalog file (teraflock/models YAML/JSON)
manifest_url  = ""              # or a catalog URL; one of the two is required for llamacpp
                                # A catalog entry's artifact_url may point at a file
                                # already on this machine, served in place (never
                                # copied or evicted): file:///models/a.gguf, a bare
                                # /models/a.gguf, or ~/models/a.gguf. Windows also
                                # takes file:///C:/models/a.gguf, C:\models\a.gguf,
                                # and \\nas\share\a.gguf; a rooted /models/a.gguf
                                # there resolves against the current drive.
default       = "mock-8b-instruct"  # model loaded at startup (behind the API, with retry);
                                # "" or "none" = nothing: the node serves what the mesh
                                # places or the operator loads; `tera up --default-model`
                                # sets it per install without editing this file
max_disk_mb   = 61440           # model-cache budget; LRU eviction below it
pin           = []              # model ids exempt from eviction (yours or the mesh's)
exclude       = []              # model ids the mesh may never place here
mesh_managed  = true            # let the coordinator place models inside max_disk_mb
                                # (download/load/evict what IT placed; never your own
                                # installs). Also a live toggle in the app/dashboard,
                                # persisted in <data_dir>/limits.toml. Off = serve only
                                # what you installed.
idle_unload_s = 900             # unload a loaded model after this many seconds without
                                # a request (0 = never). The default model is exempt.
                                # Reload is a mmap re-open (seconds), not a download;
                                # the coordinator sees the model as `cached` meanwhile.
                                # Live via PUT /api/v1/limits idle_unload_seconds.
retention_days = 0              # evict unpinned, unloaded models not used for N days
                                # (0 = never) — mesh and operator models alike. Applied
                                # on start and hourly. Live via PUT /api/v1/limits.
                                # max_disk_mb is live-settable the same way; all of
                                # these persist in <data_dir>/limits.toml.

[update]
# Version feed polled 30s after start and then hourly:
# {"flockd":{"latest","minimum","url"},"desktop":{"latest","url"}}.
# A feed that is unreachable or 404 just means "unknown" — nothing is shown.
# The daemon never self-updates: `tera status`, the TUI, the dashboard and
# the desktop app show the newer version and its release URL; brew users run
# `brew upgrade --cask tera`. `minimum` is the oldest daemon the coordinator
# still serves; below it the node is drained until updated. An enrolled node
# also gets latest/minimum/url from the coordinator itself (ConfigUpdate),
# which wins over the feed; the feed is the fallback and the URL source
# when the coordinator sends none.
feed_url = "https://api.teraflock.ai/v1/versions"

[tunnel]
coordinator_addr     = "tunnel.teraflock.ai:443"
standalone           = false    # run the in-process fake coordinator (Phase 0)
heartbeat_interval   = "5s"     # coordinator may override via HelloAck
reconnect_min        = "1s"     # jittered exponential backoff bounds
reconnect_max        = "2m"
insecure_skip_verify = false    # dev only: skip coordinator TLS verification
ca_cert              = ""       # extra root for the coordinator's server cert:
                                # a PEM file path or inline PEM. Only for a
                                # self-hosted / staging coordinator whose cert
                                # is issued by its mesh CA (or a private CA);
                                # system roots stay trusted alongside it, and
                                # tunnel.teraflock.ai needs nothing here.
insecure             = false    # dev only: plaintext gRPC to the coordinator
                                # (the `just run-coordinator` dev listener is
                                # plaintext until mTLS termination lands)

[enroll]
login_url = "https://teraflock.ai/claim"   # browser page opened by `tera login`
```

## Notes

- **Durations** use Go syntax: `"90s"`, `"2m"`, `"1h30m"`.
- **Limits set at runtime** (`tera limits`, `PUT /api/v1/limits`, web
  dashboard, desktop app) apply live and persist to `<data_dir>/limits.toml`,
  a daemon-owned overlay applied on top of `config.toml` at startup — your
  `config.toml` is never rewritten. Delete `limits.toml` to fall back to
  `config.toml`'s `[governor]`, `[models]` (`mesh_managed`, `max_disk_mb`,
  `retention_days`, `idle_unload_s`) and `[budget]` (`max_ram_mb`) values.
- **Model store hygiene**: `/api/v1/status` reports `disk{models_bytes,
  partial_bytes,budget_bytes,free_bytes,dir}` and `memory{used_mb,budget_mb,
  total_mb}`. A `.gguf` deleted outside the daemon shows as `missing` on
  `/api/v1/models` (it stops counting against the budget; the next load
  re-downloads it). `.partial` downloads older than 7 days are removed on
  start and hourly. A `<catalog-id>.gguf` found in the model dir without an
  index entry (size matching the catalog) is adopted as an operator model.
- **Multi-file models**: a sharded GGUF (catalog `parts`) or a model with
  an `mmproj` vision projector lives in `<models dir>/<id>/` under the
  upstream file names, so llama-server finds the sibling shards from part
  1 (`-m`) and gets the projector via `--mmproj`. Each file is verified
  against its own sha256; the model's `sha256` is the composite id of its
  parts, never a file hash. Shards download sequentially with per-file
  resume, `size_bytes`/disk budgeting cover the whole set, and
  `/api/v1/models` reports `parts_total`/`parts_done` next to the summed
  `received_bytes`. A complete `<id>/` set found without an index entry is
  adopted like a single file, once every file verifies. A spec that names
  neither an artifact nor parts fails with `no artifact in spec`.
- **Memory measurement** is the runtime child's physical footprint
  (`proc_pid_rusage` on macOS, `/proc/<pid>/smaps_rollup` Pss on Linux,
  `GetProcessMemoryInfo` — the larger of PrivateUsage and WorkingSetSize —
  on Windows), not RSS — mmap'd weights shared with the page cache are not
  double counted. Before the first sample a load is charged its estimate:
  `file_bytes × 1.15 + ctx × kv_bytes_per_token + 256 MB`, with the KV cost
  from the GGUF header (`file_bytes/65536` when the header lacks the
  geometry, which is 2–4x low for small GQA models) (ctx is the total
  `--ctx-size`, which llama-server splits across its slots, so the KV term
  is counted once regardless of `max_concurrent`), or the catalog's
  `min_ram_mb` if larger. On a discrete GPU the card's used memory is
  sampled every housekeeping tick (30 s, 3 s timeout) and replaces the
  estimates for admission and the heartbeat's `vram_used_mb`: NVIDIA via
  `nvidia-smi` (Linux and Windows), AMD via the amdgpu driver's sysfs
  `mem_info_vram_used` or, failing that, `rocm-smi --showmeminfo vram`
  (Linux; AMD on Windows keeps the estimate).
- **Secrets on disk** (`node.key`, `local_api_token`, `node_creds.pem`) are
  written 0600 under `data_dir`. Migration to OS keychain / DPAPI / secret
  service is a documented TODO (SPEC §A1.2).
- The artifact manifest schema for pinned llama-server builds:

```json
{
  "runtime_build_id": "llamacpp-b9892-1",
  "runtime": "llamacpp",
  "artifacts": [
    {"os": "darwin", "arch": "arm64", "accel": "metal",
     "filename": "llama-server-b9892-darwin-arm64-metal.tar.gz",
     "url": "https://teraflock-downloads.s3.amazonaws.com/runtimes/llamacpp-b9892-1/llama-server-b9892-darwin-arm64-metal.tar.gz",
     "sha256": "…", "size_bytes": 7000000}
  ]
}
```

  The tarball is verified against `sha256` before anything is unpacked;
  the daemon extracts `llama-server`, `LICENSE.llama.cpp` and `BUILDINFO`
  into `data_dir/runtimes/<runtime_build_id>/`.

## Settings and the limits API

The desktop app, the dashboard and `tera limits` never write `config.toml`:
they change settings through `GET/PUT /api/v1/limits`, which persists what
was changed in `<data_dir>/limits.toml`. **`limits.toml` overrides
`config.toml`** for the keys it holds (delete the file to undo); a key never
changed through the API stays with `config.toml`. `GET /api/v1/limits`
returns, per setting, its tier, how a change is applied, default, range and
configured vs effective value (`settings[]`).

Apply modes: **live** = immediately; **reload** = idle loaded models are
restarted with the new layout right away, a model with a request in flight
at its next load; **restart** = saved, used from the next daemon start.

| config key | limits field | tier | apply |
|---|---|---|---|
| `governor.serve_policy` | `serve_policy` | common | live |
| `governor.schedule` | `schedule` | common | live |
| `governor.idle_after` | `idle_after_seconds` | common | live |
| `governor.serve_on_battery` | `serve_on_battery` | common | live |
| `governor.max_temp_celsius` | `max_temp_celsius` | common | live |
| `models.mesh_managed` | `mesh_managed` | common | live |
| `models.max_disk_mb` | `max_disk_mb` | common | live |
| `budget.max_ram_mb` | `max_ram_mb` | common | live |
| `budget.max_concurrent` | `max_concurrent` | common | reload |
| `models.idle_unload_s` | `idle_unload_seconds` | common | live |
| `governor.yield_grace` | `yield_grace_seconds` | advanced | live |
| `models.retention_days` | `retention_days` | advanced | live |
| `runtime.max_context` | `max_context` | advanced | reload |
| `runtime.min_context` | `min_context` | advanced | reload |
| `runtime.context_length` | `context_length` | advanced | reload |
| `budget.max_vram_percent` | `max_vram_percent` | advanced | restart |
| `models.exclude` | `exclude` | advanced | live |
| `models.default` | `default_model` | advanced | restart |
| `local_api.require_auth_v1` | `require_auth_v1` | advanced | live |
| `log.level` | `log_level` | advanced | restart |

`models.pin` is edited per model (`POST /api/v1/models/{id}/pin`), not here.

File-only, on purpose:

| config key | why it is not in the app |
|---|---|
| `data_dir` | moves identity, token, models and this very overlay; a wrong value strands the node |
| `local_api.listen` | the app talks to this address; a bad bind locks it out, a public one exposes the node |
| `tunnel.coordinator_addr`, `tunnel.standalone` | which mesh the node belongs to; a wrong value takes it off the mesh |
| `tunnel.insecure`, `tunnel.insecure_skip_verify`, `tunnel.ca_cert` | transport security of the mesh session |
| `tunnel.heartbeat_interval`, `tunnel.reconnect_min`, `tunnel.reconnect_max` | protocol tuning; the coordinator overrides the heartbeat anyway |
| `runtime.kind` | `mock` serves fake output |
| `runtime.llama_server_path`, `runtime.artifact_manifest_url` | which binary the node executes |
| `runtime.artifact_signing_key`, `runtime.require_signature` | what the node trusts enough to execute |
| `runtime.mock_tokens_per_sec` | test knob |
| `models.manifest_path`, `models.manifest_url` | which catalog (artifact URLs and hashes) the node trusts |
| `enroll.login_url`, `update.feed_url` | where login and update checks go |
| `governor.poll_interval` | sampling plumbing: no operator value, and a tiny one burns CPU |
| `log.format`, `log.file` | consumed by the service manager and log tooling |
