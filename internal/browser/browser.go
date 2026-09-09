// Package browser opens URLs in the operator's default browser. One
// implementation for every caller (`tera redeem`, `tera dashboard --web`,
// the `tera login` PKCE handoff) so Windows is not left running `xdg-open`.
package browser

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
)

// Open launches url in the default browser and returns without waiting for
// it. On Windows it uses `rundll32 url.dll,FileProtocolHandler`, which works
// without a console and needs no quoting of `&` (unlike `cmd /c start`).
// Callers should print the URL on error so the operator can open it by hand.
func Open(url string) error {
	return open(command(runtime.GOOS, url))
}

// command picks the per-OS launcher. Split out so the choice is testable
// without spawning anything. The launcher is fire-and-forget by design
// (Open returns before it exits), so it deliberately runs under a
// background context rather than the caller's: a cancelled CLI context
// must not kill a browser hand-off that is already in flight.
func command(goos, url string) *exec.Cmd {
	ctx := context.Background()
	switch goos {
	case "darwin":
		return exec.CommandContext(ctx, "open", url)
	case "windows":
		return exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return exec.CommandContext(ctx, "xdg-open", url)
	}
}

func open(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("browser: open %s: %w", cmd.Path, err)
	}
	// Don't leave a zombie behind on Unix; the launcher exits immediately.
	go func() { _ = cmd.Wait() }()
	return nil
}
