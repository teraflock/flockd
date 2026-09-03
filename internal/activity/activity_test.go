package activity

import (
	"fmt"
	"testing"
	"time"

	"github.com/teraflock/flockd/internal/events"
)

func TestRingKeepsNewestFirst(t *testing.T) {
	r := New(3)
	now := time.Unix(1000, 0)
	r.clock = func() time.Time { now = now.Add(time.Second); return now }
	for i := 0; i < 5; i++ {
		r.Record(KindLoaded, ActorOperator, fmt.Sprintf("m%d", i), "loaded", "")
	}
	got := r.List()
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[0].Model != "m4" || got[1].Model != "m3" || got[2].Model != "m2" {
		t.Fatalf("order = %s %s %s, want m4 m3 m2", got[0].Model, got[1].Model, got[2].Model)
	}
	if got[0].Time.Before(got[1].Time) {
		t.Fatal("newest first violated")
	}
	if got[0].Time.IsZero() {
		t.Fatal("time not stamped")
	}
}

func TestRingPartialAndNil(t *testing.T) {
	r := New(10)
	if len(r.List()) != 0 {
		t.Fatal("empty ring listed events")
	}
	r.Record(KindEvicted, ActorDaemon, "a", "evicted", "retention")
	r.Record(KindDeclined, ActorMesh, "b", "declined", "over_memory")
	got := r.List()
	if len(got) != 2 || got[0].Model != "b" || got[1].Detail != "retention" {
		t.Fatalf("got %+v", got)
	}
	var nilRing *Ring
	nilRing.Record(KindLoaded, ActorOperator, "x", "", "") // must not panic
	if got := nilRing.List(); len(got) != 0 {
		t.Fatalf("nil ring listed %d", len(got))
	}
}

func TestRingPublishesSSE(t *testing.T) {
	hub := events.NewHub()
	ch, cancel := hub.Subscribe()
	defer cancel()
	r := New(0)
	r.Events = hub
	r.Record(KindDownloadStarted, ActorMesh, "m", "mesh started a download", "")
	select {
	case ev := <-ch:
		if ev.Type != "activity" || ev.Data.(Event).Model != "m" {
			t.Fatalf("event = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no activity event published")
	}
	if len(r.List()) != 1 {
		t.Fatal("default capacity ring lost the event")
	}
}
