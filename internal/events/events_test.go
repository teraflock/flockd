package events

import "testing"

func TestPublishSubscribe(t *testing.T) {
	h := NewHub()
	ch, cancel := h.Subscribe()
	h.Publish("ping", map[string]int{"n": 1})
	ev := <-ch
	if ev.Type != "ping" {
		t.Fatalf("type = %q", ev.Type)
	}
	cancel()
	h.Publish("after-cancel", nil) // must not block or panic
	select {
	case ev := <-ch:
		t.Fatalf("received after cancel: %+v", ev)
	default:
	}
}

func TestSlowSubscriberDropsNotBlocks(t *testing.T) {
	h := NewHub()
	_, cancel := h.Subscribe() // never read
	defer cancel()
	for i := 0; i < 200; i++ { // > buffer of 64: must not deadlock
		h.Publish("tick", i)
	}
}

func TestNilHubPublishIsSafe(t *testing.T) {
	var h *Hub
	h.Publish("noop", nil)
}
