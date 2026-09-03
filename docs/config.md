# flockd configuration

`flockd` resolves configuration in this order (later wins):

1. built-in defaults
2. TOML file — `--config <path>`, or `<data_dir>/config.toml` if it exists
3. environment variables — prefix `FLOCKD_`, `__` (double underscore) as the
   section separator: `FLOCKD_LOCAL_API__LISTEN=127.0.0.1:8080` sets
   `local_api.listen`
4. a handful of CLI flags (`--standalone`, `--runtime`, `--listen`,
   `--data-dir`, `--log-level`) override everything for convenience

Every knob, with defaults:

```toml
# Where keys, certs, tokens, models and runtimes live. 0700.
data_dir = "~/.teraflock"

[log]
level  = "info"   # debug | info | warn | error
format = "text"   # text | json

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
# Synthetic generation speed for kind=mock (tests, demos).
mock_tokens_per_sec = 120
# --ctx-size override passed to llama-server (0 = model default, capped by
# max_context).
context_length = 0
# Cap on the context window handed to llama-server, in tokens, shared across
# the budget.max_concurrent slots (0 = no cap). The KV cache scales with it:
# a 3B model at its 131072-token training window reserves ~14 GB of memory,
# at 16384 about 1.8 GB. Per-request context is max_context / max_concurrent.
max_context = 16384

[governor]
serve_policy     = "idle-only"  # always | idle-only | scheduled
idle_after       = "2m"         # input quiet time before the node counts as idle
yield_grace      = "2s"         # drain-or-cancel window on operator activity
poll_interval    = "2s"         # how often idle/power signals are sampled
serve_on_battery = false        # never serve on battery by default
max_temp_celsius = 90.0         # pause above this temperature (0 disables)
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
max_concurrent   = 2    # parallel slots

[models]
manifest_path = ""              # local catalog file (teraflock/models YAML/JSON)
manifest_url  = ""              # or a catalog URL; one of the two is required for llamacpp
default       = "mock-8b-instruct"  # model served at startup
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
# still serves; below it the node is drained until updated.
feed_url = "https://api.teraflock.ai/v1/versions"

[tunnel]
coordinator_addr     = "tunnel.teraflock.dev:443"
standalone           = false    # run the in-process fake coordinator (Phase 0)
heartbeat_interval   = "5s"     # coordinator may override via HelloAck
reconnect_min        = "1s"     # jittered exponential backoff bounds
reconnect_max        = "2m"
insecure_skip_verify = false    # dev only: skip coordinator TLS verification
insecure             = false    # dev only: plaintext gRPC to the coordinator
                                # (the `just run-coordinator` dev listener is
                                # plaintext until mTLS termination lands)

[enroll]
login_url = "https://teraflock.dev/claim"   # browser page opened by `tera login`
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
- **Memory measurement** is the runtime child's physical footprint
  (`proc_pid_rusage` on macOS, `/proc/<pid>/smaps_rollup` Pss on Linux),
  not RSS — mmap'd weights shared with the page cache are not double
  counted. Before the first sample a load is charged its estimate:
  `file_bytes × 1.15 + ctx × parallel × file_bytes/65536 + 256 MB`, or the
  catalog's `min_ram_mb` if larger.
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
