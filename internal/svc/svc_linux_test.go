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
func TestParseLinger(t *testing.T) {
	cases := []struct {
		in     string
		on, ok bool
	}{
		{"Linger=yes\n", true, true},
		{"Linger=no\n", false, true},
		{"Linger=YES", true, true},
		{"", false, false},
		{"Failed to get user: No such process\n", false, false},
		{"UID=1000\nLinger=yes\n", true, true},
	}
	for _, c := range cases {
		on, ok := parseLinger(c.in)
		if on != c.on || ok != c.ok {
			t.Errorf("parseLinger(%q) = (%v, %v), want (%v, %v)", c.in, on, ok, c.on, c.ok)
		}
	}
}

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
