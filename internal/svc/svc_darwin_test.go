//go:build darwin

package svc

import (
	"strings"
	"testing"
)

func TestRenderPlist(t *testing.T) {
	p := renderPlist("/usr/local/bin/hived", []string{"--config", "/etc/hived.toml"})
	for _, want := range []string{
		"dev.hivegrid.hived",
		"<string>/usr/local/bin/hived</string>",
		"<string>--config</string>",
		"<key>KeepAlive</key>",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plist missing %q", want)
		}
	}
}
