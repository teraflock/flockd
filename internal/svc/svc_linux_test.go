//go:build linux

package svc

import (
	"strings"
	"testing"
)

// A missing StartLimitBurst caused a real incident: on Linux boxes where
// the pinned llama.cpp catalog had no matching build, systemd restarted
// the daemon hundreds of times in ~40 minutes with no visible failure.
// Lock the caps in so a refactor cannot silently strip them.
func TestRenderUnitCapsRestartStorm(t *testing.T) {
	unit := renderUnit("/opt/flockd", []string{"--standalone"})

	for _, want := range []string{
		"StartLimitBurst=5",
		"StartLimitIntervalSec=60",
		"Restart=on-failure",
		"ExecStart=/opt/flockd --standalone",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit missing %q\n---\n%s", want, unit)
		}
	}
}
