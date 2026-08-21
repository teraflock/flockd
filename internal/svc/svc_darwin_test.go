//go:build darwin

package svc

import (
	"strings"
	"testing"
)

func TestRenderPlist(t *testing.T) {
	p := renderPlist("/usr/local/bin/flockd", []string{"--config", "/etc/flockd.toml"})
	for _, want := range []string{
		"dev.teraflock.flockd",
		"<string>/usr/local/bin/flockd</string>",
		"<string>--config</string>",
		"<key>KeepAlive</key>",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plist missing %q", want)
		}
	}
}
