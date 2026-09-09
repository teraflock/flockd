package main

import (
	"fmt"

	"github.com/teraflock/flockd/internal/localapi/client"
	"github.com/teraflock/flockd/internal/localapi/gen"
)

// client builds the shared loopback client from the persistent flags.
// Every tera command is a client of localhost:7777/api/v1 — one source of
// truth (SPEC §A1.2) — and the client itself lives in
// internal/localapi/client so the TUI, `tera mcp` and any Go tooling use
// the same one (flockd#37).
func newClient() (*client.Client, error) {
	return client.New(client.Options{API: flagAPI, DataDir: flagDataDir, Token: flagToken})
}

// dataDir resolves the daemon's data directory the way flockd does, so
// files exchanged between the two processes — the claim code, the local
// API token — land where the daemon actually looks.
func dataDir() string { return client.DataDir(flagDataDir) }

// updateLine renders the one-line update notice for status/TUI ("" = none).
func updateLine(u *gen.Update) string {
	if u == nil || !u.Available {
		return ""
	}
	line := "flockd " + u.Latest + " available"
	if u.BelowMinimum != nil && *u.BelowMinimum {
		floor := ""
		if u.Minimum != nil {
			floor = *u.Minimum
		}
		line += " (REQUIRED: below the mesh minimum " + floor + "; the node is drained until updated)"
	}
	if u.Url != nil && *u.Url != "" {
		line += " — " + *u.Url
	}
	return line
}

// gb renders bytes as GB with one decimal.
func gb(b int64) string { return fmt.Sprintf("%.1fGB", float64(b)/(1<<30)) }
