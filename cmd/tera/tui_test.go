package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/teraflock/flockd/internal/localapi/gen"
)

func fakeLogs(n int) []gen.LogEntry {
	out := make([]gen.LogEntry, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, gen.LogEntry{Time: time.Now(), Level: "INFO", Message: fmt.Sprintf("line-%03d", i)})
	}
	return out
}

func TestLogsPaneShowsNewestAndScrolls(t *testing.T) {
	m := newDashModel(nil)
	m.height = 60 // pane height clamps to 20
	m.logs = fakeLogs(50)
	m.showLogs = true

	pane := m.logsPane()
	if !strings.Contains(pane, "line-049") || strings.Contains(pane, "line-029") {
		t.Fatalf("following pane should show the newest 20 lines:\n%s", pane)
	}
	if !strings.Contains(pane, "following") {
		t.Errorf("title should say following:\n%s", pane)
	}

	m.scrollLogs(10)
	pane = m.logsPane()
	if strings.Contains(pane, "line-049") || !strings.Contains(pane, "line-039") || !strings.Contains(pane, "line-020") {
		t.Fatalf("scrolled 10 back should show 020..039:\n%s", pane)
	}
	if !strings.Contains(pane, "10 lines back") {
		t.Errorf("title should show the offset:\n%s", pane)
	}

	m.scrollLogs(1000) // clamps to oldest
	pane = m.logsPane()
	if !strings.Contains(pane, "line-000") {
		t.Fatalf("scroll should clamp at the oldest line:\n%s", pane)
	}
	m.scrollLogs(-1000)
	if m.logScroll != 0 {
		t.Fatalf("scroll should clamp at 0, got %d", m.logScroll)
	}
}

func TestLogsPaneEmpty(t *testing.T) {
	m := newDashModel(nil)
	m.showLogs = true
	if pane := m.logsPane(); !strings.Contains(pane, "nothing logged yet") {
		t.Fatalf("empty pane:\n%s", pane)
	}
}

func TestLogPaneHeightClamps(t *testing.T) {
	m := newDashModel(nil)
	m.height = 10
	if h := m.logPaneHeight(); h != 5 {
		t.Errorf("small terminal: h = %d, want 5", h)
	}
	m.height = 200
	if h := m.logPaneHeight(); h != 20 {
		t.Errorf("tall terminal: h = %d, want 20", h)
	}
}

// The MESH panel replaced a hardcoded "probation" placeholder (flockd#40):
// it must only say what /api/v1/status says.
func TestMeshPanelNeverFakesReputation(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	m := newDashModel(nil)
	m.status.Version = "0.5.0"

	m.status.Standalone = true
	if p := m.meshPanel(now); !strings.Contains(p, "standalone") || strings.Contains(p, "probation") || !strings.Contains(p, "version check pending") {
		t.Fatalf("standalone panel:\n%s", p)
	}

	exp := now.Add(27 * 24 * time.Hour)
	m.status.Standalone, m.status.Enrolled, m.status.NodeId, m.status.CertExpiresAt = false, true, "node-abcdef123456", &exp
	m.status.Update = &gen.Update{Available: true, Latest: "0.5.1"}
	m.status.Memory.UsedMb, m.status.Memory.BudgetMb = 2048, 32768
	p := m.meshPanel(now)
	for _, want := range []string{"enrolled", "node-abcdef1", "cert expires in 27d", "0.5.1 available", "2.0 / 32.0GB"} {
		if !strings.Contains(p, want) {
			t.Errorf("enrolled panel missing %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "probation") || strings.Contains(p, "reputation") {
		t.Errorf("panel mentions reputation without data:\n%s", p)
	}

	soon := now.Add(3 * 24 * time.Hour)
	m.status.CertExpiresAt = &soon
	below, minimum := true, "0.6.0"
	m.status.Update = &gen.Update{BelowMinimum: &below, Minimum: &minimum}
	p = m.meshPanel(now)
	if !strings.Contains(p, "rotation due") || !strings.Contains(p, "below mesh minimum 0.6.0") {
		t.Errorf("warnings missing:\n%s", p)
	}

	m.status.Enrolled, m.status.CertExpiresAt, m.status.Update = false, nil, nil
	if p := m.meshPanel(now); !strings.Contains(p, "not enrolled") || !strings.Contains(p, "tera login") {
		t.Errorf("not-enrolled panel:\n%s", p)
	}
}
