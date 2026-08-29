# Teraflock local dashboard

Vite + React + TypeScript + Tailwind v4 + TanStack Query, embedded into
`flockd` via `go:embed` (see `embed.go`).

- `dist/` is **committed on purpose**: `go build` must work from a fresh
  clone without Node installed. Rebuild and commit `dist/` whenever you
  touch `src/`.
- `npm install && npm run build` regenerates `dist/`.
- `npm run dev` starts a dev server that proxies `/api` and `/v1` to a
  locally running `flockd` on 127.0.0.1:7777.
- Auth: paste the bearer token from `<data_dir>/local_api_token` into the
  connect box (or `tera dashboard --web` prints it). It is stored in
  localStorage.
- The dashboard polls `/api/v1` every 2s via TanStack Query. Using the
  `/api/v1/events` SSE stream instead requires token-in-query support on
  the daemon (EventSource can't set headers) — tracked in
  docs/HANDOFF.md.
