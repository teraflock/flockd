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
# JSON manifest of pinned, SHA256-verified llama-server builds
# (see internal/runtime/llamacpp/artifact.go for the schema). Required for
# kind=llamacpp unless llama_server_path is set.
artifact_manifest_url = ""
# Synthetic generation speed for kind=mock (tests, demos).
mock_tokens_per_sec = 120
# --ctx-size override passed to llama-server (0 = model default).
context_length = 0

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
max_ram_mb       = 0    # 0 = auto
max_concurrent   = 2    # parallel slots

[models]
manifest_path = ""              # local catalog file (teraflock/models YAML/JSON)
manifest_url  = ""              # or a catalog URL; one of the two is required for llamacpp
default       = "mock-8b-instruct"  # model served at startup
max_disk_mb   = 61440           # model-cache budget; LRU eviction below it
pin           = []              # model ids exempt from eviction
exclude       = []              # model ids never assigned to this node

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
login_url = "https://teraflock.dev/claim"   # browser page opened by `flock login`
```

## Notes

- **Durations** use Go syntax: `"90s"`, `"2m"`, `"1h30m"`.
- **Limits set at runtime** (`flock limits`, `PUT /api/v1/limits`, web
  dashboard) apply live to the governor but are *not yet persisted* back to
  `config.toml`; they reset on daemon restart. TODO(persistence).
- **Secrets on disk** (`node.key`, `local_api_token`, `node_creds.pem`) are
  written 0600 under `data_dir`. Migration to OS keychain / DPAPI / secret
  service is a documented TODO (SPEC §A1.2).
- The artifact manifest schema for pinned llama-server builds:

```json
{
  "build_id": "llamacpp-b4458-flock1",
  "builds": [
    {"os": "darwin", "arch": "arm64", "accel": "metal",
     "url": "https://artifacts.teraflock.dev/llamacpp/b4458/darwin-arm64-metal/llama-server",
     "sha256": "…", "size_bytes": 4300000}
  ]
}
```
