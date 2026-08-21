# ⬡ Teraflock — `flockd`

**Turn idle GPUs into an inference mesh.** `flockd` is the open-source node
daemon for [Teraflock](https://teraflock.dev): install it on a Mac, a gaming
PC, or a homelab box, and your idle silicon serves open-weight LLM
inference — earning credits you can spend on the network's OpenAI-compatible
API or redeem for cash.

- 🖥 **One binary, three OSes.** Go daemon + CLI/TUI. llama.cpp (Metal,
  CUDA, ROCm, Vulkan, CPU) runs as a supervised subprocess.
- 🛡 **Your machine comes first.** Serves only when idle (configurable),
  never on battery by default, thermal guards, and **instant-yield**: touch
  the keyboard and in-flight work drains or cancels within 2 seconds.
  Guaranteed by tests, not vibes.
- 🔍 **Nothing to hide.** Apache-2.0, no request persistence, one outbound
  mTLS connection, hash-verified models and runtimes. Read
  [docs/privacy.md](docs/privacy.md) for exactly what the daemon can and
  cannot see — including the honest part about what node operators could
  theoretically observe.
- 💰 **Earnings you can watch tick.** `flock dashboard` (terminal) and a
  built-in web dashboard at `localhost:7777`.

> **Status: Phase 0.** The single-node vertical slice works end to end
> (see the roadmap below). The hosted coordinator/mesh is under
> construction — today the daemon runs standalone.

## Install

Coming soon (release pipeline is configured, first tagged release pending):

```sh
curl -fsSL https://get.teraflock.dev | sh        # coming soon
brew install teraflock/tap/flock                  # coming soon
winget install Teraflock.flock                    # coming soon
```

Build from source today:

```sh
git clone https://github.com/teraflock/proto ../proto   # sibling checkout (see Development)
go build ./cmd/flockd ./cmd/flock
```

## Quickstart (standalone, no account needed)

Run the daemon with the in-process fake coordinator and the deterministic
mock model — the whole serving path (governor, tunnel, telemetry, API) is
real:

```sh
flockd --standalone --runtime=mock
```

Point any OpenAI SDK at it:

```sh
export OPENAI_BASE_URL=http://localhost:7777/v1
export OPENAI_API_KEY=anything   # loopback is keyless by default
```

Python:

```python
from openai import OpenAI
client = OpenAI()  # reads OPENAI_BASE_URL / OPENAI_API_KEY

stream = client.chat.completions.create(
    model="mock-8b-instruct",
    messages=[{"role": "user", "content": "hello, mesh"}],
    stream=True,
)
for chunk in stream:
    print(chunk.choices[0].delta.content or "", end="", flush=True)
```

Node:

```js
import OpenAI from "openai";
const client = new OpenAI();

const stream = await client.chat.completions.create({
  model: "mock-8b-instruct",
  messages: [{ role: "user", content: "hello, mesh" }],
  stream: true,
});
for await (const chunk of stream) {
  process.stdout.write(chunk.choices[0]?.delta?.content ?? "");
}
```

`/v1/completions` and `/v1/embeddings` work too. To serve a **real model**,
point the daemon at a llama-server binary and a model catalog:

```toml
# ~/.teraflock/config.toml
[runtime]
kind = "llamacpp"
llama_server_path = "/opt/homebrew/bin/llama-server"

[models]
manifest_path = "/path/to/catalog.yaml"   # teraflock/models format
default = "llama-3.1-8b-instruct-q4_k_m"
```

Then watch it work:

```sh
flock dashboard        # gorgeous terminal dashboard: tok/s sparkline, earnings ticker
flock dashboard --web  # browser dashboard (prints the auth token to paste)
flock status           # one-shot status
flock limits --serve idle-only   # resource policy
scripts/smoke.sh      # the whole Phase 0 exit criterion as a script
```

## Joining the mesh (Phase 1, soon)

```sh
flock login   # browser handoff, claims this node to your account
flock up      # install + start the service (launchd / systemd --user)
```

Until the hosted coordinator ships, `flock login` stores your claim code and
the daemon serves locally only.

## Resource governance promises

The whole project dies if the daemon ever makes your machine feel slow, so
these are hard rules, enforced by the governor and its test suite:

| promise | mechanism |
|---|---|
| never serve while you're using the machine | `serve = idle-only` (default), input-idle detection |
| get out of the way *fast* | instant-yield: in-flight requests drain-or-cancel within `yield_grace` (2s default) |
| never drain your battery | `serve_on_battery = false` by default |
| never cook your laptop | `max_temp_celsius` pause threshold |
| you set the schedule | `serve = scheduled` + windows like `22:00-08:00` |
| clean exit | `flock uninstall --purge` removes everything |

## Architecture

```
OpenAI SDKs ──▶ localhost:7777/v1 ─┐
flock CLI/TUI ─▶ /api/v1 ───────────┼─▶ engine ─▶ governor ─▶ runtime (llama-server subprocess / mock)
web dashboard ▶ / (go:embed) ──────┘        ▲
coordinator ◀── mTLS tunnel (outbound only) ┘   (in-process fake coordinator in --standalone)
```

More in [docs/architecture.md](docs/architecture.md). Protocol contracts
live in [teraflock/proto](https://github.com/teraflock/proto)
(`flock.tunnel.v1`, `flock.types.v1`).

## Development

`flockd` expects the proto repo as a **sibling checkout** (there's a
`replace github.com/teraflock/proto => ../proto` in `go.mod`):

```
teraflock/
├── proto/    git clone https://github.com/teraflock/proto
└── flockd/    this repo
```

```sh
go build ./... && go vet ./... && go test ./...   # everything
scripts/smoke.sh                                  # end-to-end standalone check
cd web && npm install && npm run build            # rebuild the dashboard
```

Note: `web/dist/` is **committed on purpose** so `go build` works from a
fresh clone without Node installed (it's embedded via `go:embed`). Rebuild
it when you touch `web/src`.

Task runner (`just build|test|lint|smoke`) and CI config are in the repo;
lint with `golangci-lint run`.

## Roadmap

- [x] **Phase 0 — single-node vertical slice.** Hardware detection,
  supervised runtime, model manager, governor with instant-yield, OpenAI
  endpoint on localhost, TUI + web dashboards, fake coordinator.
  *Exit criterion met: `OPENAI_BASE_URL=http://localhost:7777/v1` works
  with the OpenAI SDKs (`scripts/smoke.sh`).*
- [ ] **Phase 1 — mesh MVP.** Real coordinator, enrollment, QUIC tunnel,
  heterogeneous nodes behind the hosted gateway.
- [ ] **Phase 2 — money.** Ledger, prepaid credits, earnings accrual +
  escrow.
- [ ] **Phase 3 — trust.** Fingerprint challenges (the daemon side already
  answers them), canary sampling, reputation, slashing.
- [ ] **Phase 4 — product.** Batch API, hosted console, redemption, signed
  auto-update.

What's implemented vs stubbed, in detail: [docs/HANDOFF.md](docs/HANDOFF.md).

## License

Apache-2.0. The daemon runs on machines we don't control; you deserve to
read every line of what it does.
