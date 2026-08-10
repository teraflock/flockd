package governor

import (
	"fmt"
	"strings"
	"time"
)

// Window is a daily serving window in local time. Overnight windows
// (start > end, e.g. 22:00-08:00) wrap across midnight.
type Window struct {
	StartMin int // minutes since midnight
	EndMin   int
}

// ParseWindow parses "HH:MM-HH:MM".
func ParseWindow(s string) (Window, error) {
	parts := strings.SplitN(strings.TrimSpace(s), "-", 2)
	if len(parts) != 2 {
		return Window{}, fmt.Errorf("governor: invalid schedule window %q (want HH:MM-HH:MM)", s)
	}
	start, err := parseHHMM(parts[0])
	if err != nil {
		return Window{}, fmt.Errorf("governor: invalid window start in %q: %w", s, err)
	}
	end, err := parseHHMM(parts[1])
	if err != nil {
		return Window{}, fmt.Errorf("governor: invalid window end in %q: %w", s, err)
	}
	return Window{StartMin: start, EndMin: end}, nil
}

// ParseWindows parses a config list like ["22:00-08:00", "12:00-13:00"].
func ParseWindows(ss []string) ([]Window, error) {
	out := make([]Window, 0, len(ss))
	for _, s := range ss {
		w, err := ParseWindow(s)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, nil
}

func parseHHMM(s string) (int, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, err
	}
	return t.Hour()*60 + t.Minute(), nil
}

// Contains reports whether t (local time-of-day) is inside the window.
func (w Window) Contains(t time.Time) bool {
	m := t.Hour()*60 + t.Minute()
	if w.StartMin <= w.EndMin {
		return m >= w.StartMin && m < w.EndMin
	}
	// Overnight wrap.
	return m >= w.StartMin || m < w.EndMin
}

func inAnyWindow(ws []Window, t time.Time) bool {
	for _, w := range ws {
		if w.Contains(t) {
			return true
		}
	}
	return false
}
