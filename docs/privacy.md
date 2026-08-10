# What the daemon can and cannot see

Trust cuts both ways. This page is for node operators ("what does this
thing do to my machine?") and for API customers ("who can read my
prompts?"). It is deliberately blunt.

## For node operators

What `hived` does on your machine:

- Detects hardware (GPU model/VRAM, CPU, RAM, disk free) and reports it to
  the coordinator so work can be scheduled sensibly. That's the
  `CapabilityProfile` — you can inspect it with `hive status`.
- Downloads model files (GGUF) and a pinned `llama-server` build into
  `~/.hivegrid`, both SHA256-verified before use. It refuses to run
  anything whose hash doesn't match the manifest.
- Opens **one outbound** mTLS connection to the coordinator. It never
  listens on a non-loopback port. No port forwarding, no inbound attack
  surface.
- Sends heartbeats every ~5s: node state, queue depth, rolling tok/s,
  temperature, battery state. No file paths, no process lists, no personal
  data.
- Serves inference when — and only when — your policy allows it
  (idle-only by default; never on battery by default; instant-yield within
  2s of you touching the machine).

What `hived` does **not** do:

- It does not read your files, your browser, or anything outside its data
  directory.
- It does not persist prompts or completions. Request content is streamed
  through memory only. There is no request logging; the local log ring
  buffer contains operational events only (model loaded, state changes,
  request *counts*).
- It does not run arbitrary code from the network. The only executables it
  fetches are pinned, hash-verified llama-server builds.
- It holds no account secrets. The node's Ed25519 private key is generated
  on your device and never leaves it.

The daemon is Apache-2.0 open source specifically so you can verify all of
the above instead of trusting this document.

Uninstall is one command and leaves nothing behind:
`hive uninstall --purge`.

## For API customers

Honesty first: **inference requires plaintext on the serving machine.**
TLS protects your prompt in transit, but the operator of a node could, in
principle, inspect their own machine's memory and read what it is
computing. Consumer GPUs have no usable trusted-execution enclave today.
Anyone who tells you otherwise is selling something.

HiveGrid's design responses:

- **Privacy tiers** (SPEC §2.1). `tier: open` (default, cheapest) may land
  on any qualified node — treat it like sending text to a well-behaved but
  unaudited third party. `tier: verified` restricts routing to
  high-reputation, staked, signed-agreement operators. `tier: private`
  runs only on first-party/partner datacenter hardware.
- **Identity stripping.** Nodes never see who you are: the coordinator
  replaces your request identity with a per-request ephemeral ID before
  dispatch. A node sees a prompt and generation parameters, nothing else.
- **No persistence, by policy and by code.** The daemon never writes
  request content to disk, and dispatches are signed by the coordinator so
  nodes can't be fed forged work.
- **Verification.** Canary sampling and model fingerprinting make it
  statistically hazardous for a node to tamper with outputs (SPEC §2.2).

Rules of thumb: batch classification of public data → `open` is fine.
Anything with PII, secrets, or regulated data → `private`, or don't send
it to a mesh at all.
