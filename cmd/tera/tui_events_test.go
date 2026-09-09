package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/teraflock/flockd/internal/localapi/client"
	"github.com/teraflock/flockd/internal/localapi/client/clienttest"
)

// run executes a tea.Cmd synchronously and returns its message.
func run(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	if cmd == nil {
		t.Fatal("nil cmd")
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		return msg
	case <-time.After(5 * time.Second):
		t.Fatal("cmd did not return within 5s")
		return nil
	}
}

// The dashboard's facts arrive over /api/v1/events (flockd#38): a status
// snapshot per event, model-list refetches on models_changed, log lines
// into the pane — against the real server.
func TestEventStreamDrivesDashboard(t *testing.T) {
	d := clienttest.New(t, clienttest.Options{})
	m := newDashModel(chatClient(t, d))
	defer m.cancel()

	msg := run(t, m.connectCmd())
	sm, ok := msg.(streamMsg)
	if !ok || sm.err != nil {
		t.Fatalf("connect = %#v", msg)
	}
	m.Update(sm)
	if !m.connected || m.stream == nil || m.backoff != backoffMin {
		t.Fatalf("after connect: connected=%v backoff=%s", m.connected, m.backoff)
	}

	// First event is the status snapshot.
	ev, ok := run(t, readCmd(m.stream)).(eventMsg)
	if !ok || ev.ev.Name != "status" {
		t.Fatalf("first event = %#v", ev)
	}
	m.Update(ev)
	if !m.haveOne || m.status.State != "serving" || len(m.spark) != 1 {
		t.Fatalf("status not applied: haveOne=%v state=%q spark=%v", m.haveOne, m.status.State, m.spark)
	}

	// models_changed triggers a refetch; the refetch lands.
	d.Events.Publish("models_changed", map[string]string{"model": "m1", "change": "loaded"})
	for {
		ev = run(t, readCmd(m.stream)).(eventMsg)
		if ev.ev.Name == "models_changed" {
			break
		}
	}
	cmd := m.applyEvent(ev.ev)
	if cmd == nil {
		t.Fatal("models_changed should schedule a model-list refetch")
	}
	mm, ok := run(t, cmd).(modelsMsg)
	if !ok || mm.err != nil {
		t.Fatalf("refetch = %#v", mm)
	}

	// A log line lands in the pane without any fetch.
	d.Log.Info("streamed line")
	for {
		ev = run(t, readCmd(m.stream)).(eventMsg)
		if ev.ev.Name == "log" {
			break
		}
	}
	m.Update(ev)
	if len(m.logs) != 1 || m.logs[0].Message != "streamed line" {
		t.Fatalf("logs = %+v", m.logs)
	}

	// Quitting cancels the stream; the end message is then a no-op.
	m.cancel()
	end, ok := run(t, readCmd(m.stream)).(streamEndMsg)
	if !ok {
		t.Fatalf("after cancel = %#v", end)
	}
	if _, cmd := m.Update(end); cmd != nil {
		t.Fatal("no reconnect after quit")
	}
}

func TestReconnectBackoffDoublesAndCaps(t *testing.T) {
	m := newDashModel(nil)
	defer m.cancel()
	m.now = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30}
	for i, w := range want {
		_, cmd := m.Update(streamMsg{err: errors.New("dial tcp: connection refused")})
		if cmd == nil || m.connected {
			t.Fatalf("attempt %d: no reconnect scheduled", i)
		}
		if got := m.retryAt.Sub(m.now); got != w*time.Second {
			t.Fatalf("attempt %d: wait = %s, want %s", i, got, w*time.Second)
		}
	}
	m.haveOne = true
	if v := m.View(); !strings.Contains(v, "reconnecting in") || !strings.Contains(v, "connection refused") {
		t.Fatalf("footer should explain the outage:\n%s", v)
	}

	// The daemon closing the stream also reconnects, with a reason.
	m.Update(streamEndMsg{})
	if m.streamErr == nil || !strings.Contains(m.reconnectLine(), "closed") {
		t.Fatalf("stream end: %v / %s", m.streamErr, m.reconnectLine())
	}

	// A successful connect resets the backoff.
	m.Update(streamMsg{s: &client.Stream{}})
	if !m.connected || m.backoff != backoffMin || m.streamErr != nil {
		t.Fatalf("after reconnect: connected=%v backoff=%s err=%v", m.connected, m.backoff, m.streamErr)
	}
	if v := m.View(); strings.Contains(v, "reconnecting") || !strings.Contains(v, "live") {
		t.Fatalf("footer should be live again:\n%s", v)
	}
}

func TestPollSamplesOnlyWhileDisconnected(t *testing.T) {
	m := newDashModel(nil)
	defer m.cancel()
	var d dataMsg
	d.status.Stats.TokensPerSec1m = 3
	m.connected = true
	m.Update(d)
	if len(m.spark) != 0 {
		t.Fatal("poll must not add samples while the stream feeds them")
	}
	m.connected = false
	m.Update(d)
	if len(m.spark) != 1 || m.spark[0] != 3 {
		t.Fatalf("spark = %v", m.spark)
	}
}

func TestModelProgressRefetchIsThrottled(t *testing.T) {
	m := newDashModel(nil)
	defer m.cancel()
	m.now = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	ev := client.Event{Name: "model_progress", Data: json.RawMessage(`{"model":"m","received_bytes":1,"total_bytes":2}`)}
	if m.applyEvent(ev) == nil {
		t.Fatal("first progress event should refetch")
	}
	if m.applyEvent(ev) != nil {
		t.Fatal("second progress event within 1s should not")
	}
	m.now = m.now.Add(2 * time.Second)
	if m.applyEvent(ev) == nil {
		t.Fatal("after the throttle window it should refetch again")
	}
}

func TestStatusEventFeedsSparklineWindow(t *testing.T) {
	m := newDashModel(nil)
	defer m.cancel()
	for i := 0; i < 70; i++ {
		m.applyEvent(client.Event{Name: "status", Data: json.RawMessage(`{"state":"serving","stats":{"tokens_per_sec_1m":1}}`)})
	}
	if len(m.spark) != 60 || !m.haveOne || m.status.State != "serving" {
		t.Fatalf("spark=%d haveOne=%v state=%q", len(m.spark), m.haveOne, m.status.State)
	}
}
