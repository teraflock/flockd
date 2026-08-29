package logging

import (
	"testing"
	"time"
)

func TestRingCapturesAndTails(t *testing.T) {
	log, ring := New("debug", "text")
	log.Info("first", "k", "v")
	log.Warn("second")
	log.Debug("third")

	entries := ring.Tail(0)
	if len(entries) != 3 {
		t.Fatalf("entries = %d", len(entries))
	}
	if entries[0].Message != "first" || entries[0].Attrs != "k=v" {
		t.Errorf("entry[0] = %+v", entries[0])
	}
	if entries[1].Level != "WARN" {
		t.Errorf("entry[1] = %+v", entries[1])
	}

	tail := ring.Tail(2)
	if len(tail) != 2 || tail[0].Message != "second" {
		t.Errorf("tail = %+v", tail)
	}
}

func TestRingWraps(t *testing.T) {
	r := NewRing(4)
	for i := 0; i < 10; i++ {
		r.add(Entry{Message: string(rune('a' + i))})
	}
	got := r.Tail(0)
	if len(got) != 4 {
		t.Fatalf("len = %d", len(got))
	}
	if got[0].Message != "g" || got[3].Message != "j" {
		t.Errorf("wrapped tail = %+v", got)
	}
}

func TestLevelFiltering(t *testing.T) {
	log, ring := New("warn", "json")
	log.Info("hidden")
	log.Error("shown")
	entries := ring.Tail(0)
	if len(entries) != 1 || entries[0].Message != "shown" {
		t.Errorf("entries = %+v", entries)
	}
}

func TestRingSubscribeFollows(t *testing.T) {
	log, ring := New("info", "text")
	ch, cancel := ring.Subscribe()
	defer cancel()
	log.Info("hello follower", "k", "v")
	select {
	case e := <-ch:
		if e.Message != "hello follower" || e.Attrs != "k=v" {
			t.Fatalf("entry = %+v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no entry delivered to subscriber")
	}
	cancel()
	log.Info("after cancel")
	select {
	case e := <-ch:
		if e.Message == "after cancel" {
			t.Fatal("received after cancel")
		}
	default:
	}
}
