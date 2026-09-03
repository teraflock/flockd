package update

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/teraflock/flockd/internal/activity"
	"github.com/teraflock/flockd/internal/events"
)

func TestCompareAndIsRelease(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.4.0", "0.3.0", 1},
		{"v0.4.0", "0.4.0", 0},
		{"0.3.9", "0.4.0", -1},
		{"0.10.0", "0.9.0", 1},
		{"1.0.0", "0.99.99", 1},
		{"ad2a146-dev", "0.4.0", -1},
		{"dev", "dev", 0},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
	for _, v := range []string{"0.3.0", "v1.2.3"} {
		if !IsRelease(v) {
			t.Errorf("IsRelease(%q) = false", v)
		}
	}
	for _, v := range []string{"dev", "ad2a146-dev", "0.4.0-rc1", "0.4", ""} {
		if IsRelease(v) {
			t.Errorf("IsRelease(%q) = true", v)
		}
	}
}

func feed(latest, minimum string) Feed {
	var f Feed
	f.Flockd.Latest, f.Flockd.Minimum, f.Flockd.URL = latest, minimum, "https://github.com/teraflock/flockd/releases/tag/v"+latest
	return f
}

func TestEvaluate(t *testing.T) {
	r := Evaluate("0.3.0", feed("0.4.0", "0.3.0"), time.Now())
	if !r.Available || r.BelowMinimum || r.Latest != "0.4.0" || r.URL == "" {
		t.Fatalf("0.3.0 vs 0.4.0: %+v", r)
	}
	r = Evaluate("0.2.1", feed("0.4.0", "0.3.0"), time.Now())
	if !r.Available || !r.BelowMinimum {
		t.Fatalf("0.2.1 below minimum: %+v", r)
	}
	r = Evaluate("0.4.0", feed("0.4.0", "0.3.0"), time.Now())
	if r.Available || r.BelowMinimum {
		t.Fatalf("up to date: %+v", r)
	}
	// Dev builds are never nagged, but still learn what's out there.
	r = Evaluate("ad2a146-dev", feed("0.4.0", "0.3.0"), time.Now())
	if r.Available || r.BelowMinimum || r.Latest != "0.4.0" {
		t.Fatalf("dev build: %+v", r)
	}
}

func TestCheckAnnouncesOncePerVersion(t *testing.T) {
	body := `{"flockd":{"latest":"0.4.0","minimum":"0.3.0","url":"https://example/v0.4.0"},"desktop":{"latest":"1.0.0","url":"x"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	hub := events.NewHub()
	ch, cancel := hub.Subscribe()
	defer cancel()
	ring := activity.New(10)
	c := &Checker{FeedURL: srv.URL, Current: "0.3.0", Events: hub, Activity: ring}
	if _, ok := c.Last(); ok {
		t.Fatal("Last before any check")
	}
	for i := 0; i < 2; i++ {
		res, err := c.Check(context.Background())
		if err != nil || !res.Available {
			t.Fatalf("check %d: %+v %v", i, res, err)
		}
	}
	select {
	case ev := <-ch:
		if ev.Type != "update_available" || ev.Data.(Public).Latest != "0.4.0" {
			t.Fatalf("event = %+v", ev)
		}
	default:
		t.Fatal("no update_available event")
	}
	select {
	case ev := <-ch:
		t.Fatalf("second check re-announced: %+v", ev)
	default:
	}
	if got := ring.List(); len(got) != 1 || got[0].Kind != activity.KindUpdateAvailable {
		t.Fatalf("activity = %+v", got)
	}
	// A newer latest is announced again.
	body = `{"flockd":{"latest":"0.5.0","minimum":"0.3.0","url":"u"}}`
	if _, err := c.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-ch:
		if ev.Data.(Public).Latest != "0.5.0" {
			t.Fatalf("event = %+v", ev)
		}
	default:
		t.Fatal("0.5.0 not announced")
	}
	if last, ok := c.Last(); !ok || last.Latest != "0.5.0" {
		t.Fatalf("Last = %+v %v", last, ok)
	}
}

func TestFeedUnavailableIsUnknownNotError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	c := &Checker{FeedURL: srv.URL, Current: "0.3.0"}
	if _, err := c.Check(context.Background()); !errors.Is(err, ErrFeedUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if _, ok := c.Last(); ok {
		t.Fatal("failed check recorded a result")
	}
	srv.Close()
	if _, err := c.Check(context.Background()); !errors.Is(err, ErrFeedUnavailable) {
		t.Fatalf("connection error: %v", err)
	}
}
