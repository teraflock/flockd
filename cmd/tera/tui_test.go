package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func fakeLogs(n int) []logEntry {
	out := make([]logEntry, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, logEntry{Time: time.Now(), Level: "INFO", Message: fmt.Sprintf("line-%03d", i)})
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
