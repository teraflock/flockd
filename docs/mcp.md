# `tera mcp` — your node as an MCP server

`tera mcp` runs a [Model Context Protocol](https://modelcontextprotocol.io)
server on stdin/stdout that exposes the Teraflock node on **this machine**
to any MCP client on it — Claude Code, Claude Desktop, Cursor. Point your
agent tooling at the machine already in your closet: it can chat with the
models you have installed, see what is loaded, and pull, load or unload
models, all through the daemon's loopback API.

What it is, precisely:

- A subcommand of `tera`, spawned by the MCP client as **you**, that talks
  to `flockd` at `http://127.0.0.1:7777` with the same bearer token every
  other `tera` command uses (`<data_dir>/local_api_token`, 0600).
- **Loopback only.** It opens no port. The daemon's bind is unchanged. If a
  process on your machine can run `tera`, it can use this; nothing else can.
- A thin wrapper over endpoints that already exist (`/api/v1/*` and
  `/v1/chat/completions`). Every path it wraps is in
  [`api/openapi.yaml`](../api/openapi.yaml); a test enforces that.
- Inference runs on your hardware and never leaves the machine. Nothing is
  logged from the conversation (see [privacy.md](privacy.md)).

It runs until the client closes stdin. You never start it by hand.

## What it exposes

`tera mcp --describe` prints this list from the running server, so it is
always current:

```
Tools:
  chat             Run a chat completion on a model served by this node (local, private, no API key). Non-streaming; returns the text, usage and tok/s.
  download_model   Start downloading a catalog model in the background (see the teraflock://catalog resource for ids). Returns immediately; progress shows in node_status and list_models.
  list_models      List the models installed on this node with state (downloading/ready/missing), whether they are loaded, and which is the default.
  load_model       Load an installed model into the serving runtime (synchronous; downloads first if it is not cached). Needs the llama.cpp runtime.
  node_status      This node's state (serving or why not), loaded models, memory and disk budgets, enrollment, update status, and downloads in flight.
  unload_model     Unload a model from the runtime; the file stays cached on disk.
Resources:
  teraflock://catalog    The Teraflock model catalog merged with this node's local state (installed, loaded, default). JSON.
  teraflock://logs       The last 100 lines of the daemon's log ring (request content is never logged). Text.
  teraflock://status     The full /api/v1/status snapshot: hardware, stats, memory, disk, update. JSON.
```

`chat` takes `prompt` (one user message) or `messages` (a full OpenAI-style
conversation), plus optional `model` (default: the node's default model),
`system`, `max_tokens` (daemon default 256) and `temperature`. It returns
the answer as text and, as structured content, the model id, usage
(`prompt_tokens`, `completion_tokens`, `total_tokens`), `tokens_per_sec`,
`elapsed_ms`, and `reasoning` when the model produced chain-of-thought.
It is not streamed — MCP tool results are not streams — so a long answer
arrives when it is finished. The first call on a cold model takes a few
seconds while the daemon loads it (models unload again after
`models.idle_unload_s`, default 900 s).

`load_model`, `unload_model` and `download_model` change the node. They
need the llama.cpp runtime (`runtime.kind = "llamacpp"`); the mock runtime
answers every one with an explanation.

## Claude Code

One command, user-wide:

```sh
claude mcp add --scope user teraflock -- tera mcp
```

Or per project, in a `.mcp.json` at the repo root (checked in, so the
whole team gets it):

```json
{
  "mcpServers": {
    "teraflock": {
      "command": "tera",
      "args": ["mcp"]
    }
  }
}
```

Check it with `claude mcp list` (or `/mcp` inside Claude Code), then ask:
"list my node's models", "ask my local model for a haiku about idle GPUs".
Each becomes a tool call — `list_models`, then `chat` — and the answer
comes back with the token counts and tok/s.

## Claude Desktop

Claude Desktop reads `claude_desktop_config.json`:

| OS | path |
|---|---|
| macOS | `~/Library/Application Support/Claude/claude_desktop_config.json` |
| Windows | `%APPDATA%\Claude\claude_desktop_config.json` |

Add the server with the **absolute path** to `tera` — GUI apps on macOS do
not inherit your shell's `PATH`, so a bare `tera` fails with "command not
found" while the same line works in Terminal. `which tera` prints the path
(`/opt/homebrew/bin/tera` for Homebrew on Apple silicon,
`/usr/local/bin/tera` on Intel Macs, `C:\Program Files\Teraflock\tera.exe`
or wherever you unzipped it on Windows):

```json
{
  "mcpServers": {
    "teraflock": {
      "command": "/opt/homebrew/bin/tera",
      "args": ["mcp"]
    }
  }
}
```

Restart Claude Desktop. The tools show under the attachments (+) menu →
Connectors / MCP servers; the hammer icon lists the six tools.

## Cursor

Global: `~/.cursor/mcp.json`. Per project: `.cursor/mcp.json` in the repo.
Same shape; use the absolute path here too, Cursor is a GUI app:

```json
{
  "mcpServers": {
    "teraflock": {
      "command": "/opt/homebrew/bin/tera",
      "args": ["mcp"]
    }
  }
}
```

Cursor → Settings → MCP shows the server with a green dot and the tool
list once it has spawned it.

## Non-default daemon address or data dir

`tera mcp` takes the same persistent flags as every `tera` command, and the
client passes them through:

```json
{ "command": "/opt/homebrew/bin/tera", "args": ["mcp", "--api", "http://127.0.0.1:8080", "--data-dir", "/srv/teraflock"] }
```

`TERA_TOKEN` and `FLOCKD_DATA_DIR` work as environment variables too, where
the client supports them (Claude Code: `-e KEY=value`; the JSON files: an
`"env": {}` object next to `"args"`).

## Or skip MCP: the OpenAI-compatible endpoint

Tools that speak the OpenAI API — Cursor's model settings, Zed, Continue,
Open WebUI, the `openai` SDKs — need no MCP at all. Point them at the
daemon's loopback endpoint; any API key string is accepted on loopback:

```
base URL  http://127.0.0.1:7777/v1
API key   anything
model     the id from `tera models list`
```

The same rule applies: loopback only, unless you set `local_api.listen` to
something else, which [config.md](config.md) tells you not to do lightly.

## Troubleshooting

Every error the tools return says what to do; these are the three you will
actually hit.

| symptom | cause | fix |
|---|---|---|
| `cannot reach flockd at http://127.0.0.1:7777` | the daemon is not running | `tera up` (or `flockd --standalone --runtime=mock` to try it without an account) |
| `daemon error (503): node is not serving right now (yielded)` | the default `serve_policy = idle-only`: the node serves only while you are away from the keyboard, and you are at it | `tera limits --serve always`, or wait for the node to go idle; `node_status` explains the state in words |
| `daemon rejected the auth token` | `tera` looked in a different data dir than the daemon uses | pass `--data-dir` (or `FLOCKD_DATA_DIR` / `TERA_TOKEN`) to `tera mcp`; `tera token` prints the current token |

Other checks:

- `tera mcp --describe` — the server starts and lists its surface (it
  resolves the daemon client but does not call the daemon, so it works with
  the daemon down).
- `npx @modelcontextprotocol/inspector tera mcp` — the MCP Inspector's UI
  against the real server; `--cli --method tools/list` for a one-liner.
- `daemon error (501): model operations need the llamacpp runtime` — the
  node runs the mock runtime; `chat` works, model management does not.
- Claude Desktop / Cursor show nothing: the config path or the JSON is
  wrong, or `command` is not absolute. Their logs name the failing server
  (`~/Library/Logs/Claude/mcp*.log` on macOS).

## What is not here

- No hosted MCP server in this repo: that is the gateway's job
  (`api.teraflock.ai`, with an API key) and lives in the control plane.
- No MCP over HTTP on port 7777, and no streaming tool output. Both are
  possible; neither is needed to make the local case work.
