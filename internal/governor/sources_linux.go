//go:build linux

package governor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// NewPlatformIdleSource returns the Linux idle source: systemd-logind's
// IdleHint / IdleSinceHint for the operator's seat session, read through
// `loginctl show-session`. The session is $XDG_SESSION_ID when the daemon
// runs inside a login session, else the first active seat session from
// `loginctl list-sessions` (a systemd --user service has no
// XDG_SESSION_ID). No logind, or no seat session at all (headless), is
// ErrNoIdleSource: the governor assumes idle with a one-time log line,
// which is the right default for servers.
//
// Caveat: IdleHint is set by the desktop session (GNOME, KDE, sway with
// an idle daemon…); a bare X session without one leaves it at "no", so
// the node never counts as idle there — serve_policy = always or
// scheduled is the operator's answer.
func NewPlatformIdleSource() IdleSource { return &linuxIdleSource{} }

type linuxIdleSource struct {
	mu      sync.Mutex
	session string // resolved session id, "" until first success
}

func (s *linuxIdleSource) IdleFor(ctx context.Context) (time.Duration, error) {
	sid, err := s.sessionID(ctx)
	if err != nil {
		return 0, err
	}
	out, err := exec.CommandContext(ctx, "loginctl", "show-session", sid, "-p", "IdleHint", "-p", "IdleSinceHint").Output()
	if err != nil {
		// The session may have ended (logout): resolve again next time.
		s.mu.Lock()
		s.session = ""
		s.mu.Unlock()
		return 0, fmt.Errorf("governor: loginctl show-session %s: %w", sid, err)
	}
	return parseLoginctlSession(string(out), time.Now())
}

func (s *linuxIdleSource) sessionID(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session != "" {
		return s.session, nil
	}
	if sid := strings.TrimSpace(os.Getenv("XDG_SESSION_ID")); sid != "" {
		s.session = sid
		return sid, nil
	}
	out, err := exec.CommandContext(ctx, "loginctl", "list-sessions", "--no-legend").Output()
	if err != nil {
		return "", fmt.Errorf("%w (loginctl: %v)", ErrNoIdleSource, err)
	}
	sid, err := pickLoginctlSession(string(out))
	if err != nil {
		return "", fmt.Errorf("%w (%v)", ErrNoIdleSource, err)
	}
	s.session = sid
	return sid, nil
}

// NewPlatformPowerSource reads /sys/class/power_supply and thermal zones.
func NewPlatformPowerSource() PowerSource { return linuxPowerSource{root: "/sys"} }

type linuxPowerSource struct{ root string }

func (s linuxPowerSource) Status(context.Context) (PowerStatus, error) {
	ps := PowerStatus{}
	// AC online? Any ACAD/AC* supply with online=1 means not on battery.
	supplies, _ := filepath.Glob(filepath.Join(s.root, "class/power_supply/*"))
	onAC := len(supplies) == 0 // desktops without a battery: treat as AC
	for _, sup := range supplies {
		typ, _ := os.ReadFile(filepath.Join(sup, "type"))
		if strings.TrimSpace(string(typ)) == "Mains" {
			online, _ := os.ReadFile(filepath.Join(sup, "online"))
			if strings.TrimSpace(string(online)) == "1" {
				onAC = true
			}
		}
	}
	ps.OnBattery = !onAC

	// Best-effort temperature from the hottest thermal zone.
	zones, _ := filepath.Glob(filepath.Join(s.root, "class/thermal/thermal_zone*/temp"))
	for _, z := range zones {
		raw, err := os.ReadFile(z)
		if err != nil {
			continue
		}
		var milli int64
		if _, err := fmt.Sscanf(strings.TrimSpace(string(raw)), "%d", &milli); err == nil {
			if c := float64(milli) / 1000; c > ps.TempCelsius {
				ps.TempCelsius = c
			}
		}
	}
	return ps, nil
}
