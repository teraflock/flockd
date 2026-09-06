package governor

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// systemd-logind is the Linux idle signal: every seat session carries
// IdleHint (set by the desktop's screensaver / idle monitor) and
// IdleSinceHint (µs since the epoch, 0 = unknown). The parsers live here,
// build-tag free, so the fixture tests run everywhere; sources_linux.go
// does the exec.

// idleSinceUnknown is what a session that says "idle" without saying
// since when counts as: long enough for any idle_after policy.
const idleSinceUnknown = 24 * time.Hour

// errNoSeatSession means loginctl listed no active seat session (headless
// box, or the daemon runs outside any graphical login).
var errNoSeatSession = errors.New("governor: no active seat session in loginctl list-sessions")

// parseLoginctlSession reads `loginctl show-session <id> -p IdleHint
// -p IdleSinceHint` output:
//
//	IdleHint=yes
//	IdleSinceHint=1757100000000000
//
// and returns how long the session has been idle (0 when not idle).
func parseLoginctlSession(out string, now time.Time) (time.Duration, error) {
	var hint string
	var since int64
	haveHint := false
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "IdleHint":
			hint, haveHint = strings.ToLower(strings.TrimSpace(v)), true
		case "IdleSinceHint":
			since, _ = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		}
	}
	if !haveHint {
		return 0, fmt.Errorf("governor: IdleHint not in loginctl output")
	}
	if hint != "yes" {
		return 0, nil
	}
	if since <= 0 {
		return idleSinceUnknown, nil
	}
	d := now.Sub(time.UnixMicro(since))
	if d < 0 {
		d = 0
	}
	return d, nil
}

// pickLoginctlSession chooses the session to watch from `loginctl
// list-sessions --no-legend` output — the first session on a seat that
// is active (the STATE column, when the systemd version prints one):
//
//	SESSION UID USER SEAT TTY [STATE IDLE SINCE]
//	      3 1000 anderson seat0 tty2 active no -
//	      5 1000 anderson - pts/1 active no -
//
// Seat sessions are the ones logind tracks input for; an ssh session
// (no seat) says nothing about whether the operator is at the keyboard.
func pickLoginctlSession(out string) (string, error) {
	var fallback string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		id, seat := f[0], f[3]
		if seat == "" || seat == "-" {
			continue
		}
		if len(f) >= 6 && f[5] != "active" && f[5] != "online" {
			if fallback == "" {
				fallback = id
			}
			continue
		}
		return id, nil
	}
	if fallback != "" {
		return fallback, nil
	}
	return "", errNoSeatSession
}

// idleFromTicks turns Windows GetLastInputInfo's dwTime (the low 32 bits
// of the tick count at the last input) and the current tick count into
// an idle duration, correct across the 49.7-day 32-bit wrap.
func idleFromTicks(nowTicks uint64, lastInput uint32) time.Duration {
	return time.Duration(uint32(nowTicks)-lastInput) * time.Millisecond
}
