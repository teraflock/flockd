//go:build darwin

package svc

import (
	"strings"
	"testing"
)

func TestRenderPlist(t *testing.T) {
	p := renderPlist("/usr/local/bin/flockd", []string{"--config", "/etc/flockd.toml"},
		Options{LogPath: "/Users/x/.teraflock/flockd.log"})
	for _, want := range []string{
		"dev.teraflock.flockd",
		"<string>/usr/local/bin/flockd</string>",
		"<string>--config</string>",
		"<key>KeepAlive</key>",
		"<key>StandardOutPath</key>",
		"<string>/Users/x/.teraflock/flockd.log</string>",
		// Standard, never Background: Background QoS throttled first-run
		// model downloads ~20x (see renderPlist comment).
		"<string>Standard</string>",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plist missing %q", want)
		}
	}
	if strings.Contains(p, "LowPriorityBackgroundIO") {
		t.Error("LowPriorityBackgroundIO must stay gone (I/O throttle)")
	}
	if noLog := renderPlist("/bin/flockd", nil, Options{}); strings.Contains(noLog, "StandardOutPath") {
		t.Error("log keys rendered without a LogPath")
	}
}
