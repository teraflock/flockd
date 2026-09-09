package governor

import (
	"errors"
	"math"
	"testing"
	"time"
)

func TestParseLoginctlSession(t *testing.T) {
	now := time.UnixMicro(1757100000000000).Add(90 * time.Second)
	d, err := parseLoginctlSession("IdleHint=yes\nIdleSinceHint=1757100000000000\n", now)
	if err != nil || d != 90*time.Second {
		t.Fatalf("idle 90s: %v, %v", d, err)
	}
	if d, err := parseLoginctlSession("IdleHint=no\nIdleSinceHint=1757100000000000\n", now); err != nil || d != 0 {
		t.Fatalf("not idle: %v, %v", d, err)
	}
	// Idle but logind does not know since when: idle long enough.
	if d, err := parseLoginctlSession("IdleHint=yes\nIdleSinceHint=0\n", now); err != nil || d != idleSinceUnknown {
		t.Fatalf("idle since unknown: %v, %v", d, err)
	}
	// Clock skew never yields a negative idle.
	if d, err := parseLoginctlSession("IdleHint=yes\nIdleSinceHint=1757100000000000\n", now.Add(-time.Hour)); err != nil || d != 0 {
		t.Fatalf("future since: %v, %v", d, err)
	}
	if _, err := parseLoginctlSession("Id=3\nState=active\n", now); err == nil {
		t.Fatal("missing IdleHint accepted")
	}
}

func TestParseLoginctlSessionLockedHint(t *testing.T) {
	now := time.UnixMicro(1757100000000000).Add(90 * time.Second)
	// Locked screen: idle regardless of IdleHint (a bare X session never
	// sets it), and at least idleSinceUnknown so any idle_after passes.
	for _, out := range []string{
		"IdleHint=no\nIdleSinceHint=0\nLockedHint=yes\n",
		"IdleHint=yes\nIdleSinceHint=1757100000000000\nLockedHint=yes\n",
		"IdleHint=yes\nIdleSinceHint=0\nLockedHint=yes\n",
	} {
		if d, err := parseLoginctlSession(out, now); err != nil || d != idleSinceUnknown {
			t.Errorf("%q: %v, %v; want %v", out, d, err, idleSinceUnknown)
		}
	}
	// An idle stretch longer than idleSinceUnknown is not shortened by the lock.
	long := "IdleHint=yes\nIdleSinceHint=1757100000000000\nLockedHint=yes\n"
	if d, err := parseLoginctlSession(long, now.Add(48*time.Hour)); err != nil || d < 48*time.Hour {
		t.Errorf("long idle + locked: %v, %v", d, err)
	}
	// LockedHint=no changes nothing: IdleHint/IdleSinceHint rule.
	if d, err := parseLoginctlSession("IdleHint=no\nIdleSinceHint=0\nLockedHint=no\n", now); err != nil || d != 0 {
		t.Errorf("unlocked, not idle: %v, %v", d, err)
	}
	if d, err := parseLoginctlSession("IdleHint=yes\nIdleSinceHint=1757100000000000\nLockedHint=no\n", now); err != nil || d != 90*time.Second {
		t.Errorf("unlocked, idle 90s: %v, %v", d, err)
	}
	// Missing LockedHint (old systemd) behaves exactly as before.
	if d, err := parseLoginctlSession("IdleHint=yes\nIdleSinceHint=1757100000000000\n", now); err != nil || d != 90*time.Second {
		t.Errorf("no LockedHint: %v, %v", d, err)
	}
	// Still an error without IdleHint, lock or not.
	if _, err := parseLoginctlSession("LockedHint=yes\n", now); err == nil {
		t.Error("LockedHint alone accepted without IdleHint")
	}
}

func TestPickLoginctlSession(t *testing.T) {
	// systemd ≥ 255 prints STATE/IDLE/SINCE; the ssh session has no seat.
	modern := "      5 1000 anderson -     pts/1 active no -\n      3 1000 anderson seat0 tty2  active no -\n"
	if id, err := pickLoginctlSession(modern); err != nil || id != "3" {
		t.Fatalf("modern: %q, %v", id, err)
	}
	// Older systemd: SESSION UID USER SEAT TTY only.
	old := "3 1000 anderson seat0 tty2\n"
	if id, err := pickLoginctlSession(old); err != nil || id != "3" {
		t.Fatalf("old: %q, %v", id, err)
	}
	// A closing seat session is only a fallback behind an active one.
	mixed := "      2 1000 anderson seat0 tty2 closing no -\n      7 1000 anderson seat0 tty3 active no -\n"
	if id, err := pickLoginctlSession(mixed); err != nil || id != "7" {
		t.Fatalf("mixed: %q, %v", id, err)
	}
	if id, err := pickLoginctlSession("      2 1000 anderson seat0 tty2 closing no -\n"); err != nil || id != "2" {
		t.Fatalf("fallback: %q, %v", id, err)
	}
	// Headless: only ssh sessions.
	if _, err := pickLoginctlSession("      5 1000 anderson - pts/1 active no -\n"); !errors.Is(err, errNoSeatSession) {
		t.Fatalf("headless: %v", err)
	}
	if _, err := pickLoginctlSession(""); !errors.Is(err, errNoSeatSession) {
		t.Fatalf("empty: %v", err)
	}
}

func TestIdleFromTicksWraps(t *testing.T) {
	if d := idleFromTicks(10_000, 4_000); d != 6*time.Second {
		t.Fatalf("plain: %v", d)
	}
	// Last input just before the 32-bit tick counter wrapped.
	if d := idleFromTicks(uint64(math.MaxUint32)+1+500, math.MaxUint32-499); d != time.Second {
		t.Fatalf("wrap: %v", d)
	}
	// GetTickCount64 past 49.7 days still compares in the low 32 bits.
	now := uint64(math.MaxUint32) + 1 + 120_000
	if d := idleFromTicks(now, uint32(now)-120_000); d != 2*time.Minute {
		t.Fatalf("64-bit now: %v", d)
	}
}
