package main

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/teraflock/flockd/internal/localapi/client/clienttest"
)

// Runs the real bubbletea program headlessly against the real server: the
// stream must keep delivering status snapshots (2s cadence) for as long as
// the dashboard runs, not just the first one.
func TestDashboardProgramStaysLive(t *testing.T) {
	d := clienttest.New(t, clienttest.Options{})
	m := newDashModel(chatClient(t, d))
	defer m.cancel() // `q` does this in the real program; Quit() here bypasses the key handler
	p := tea.NewProgram(m, tea.WithoutRenderer(), tea.WithInput(nil))
	done := make(chan error, 1)
	go func() { _, err := p.Run(); done <- err }()
	time.Sleep(5500 * time.Millisecond)
	p.Quit()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("program did not quit")
	}
	if !m.connected || len(m.spark) < 3 {
		t.Fatalf("connected=%v samples=%d (want the initial snapshot plus one per 2s)", m.connected, len(m.spark))
	}
}
