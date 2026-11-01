package governor

import (
	"context"
	"errors"
	"testing"
	"time"
)

func awakePolicy(serve string) Policy {
	p := idleOnlyPolicy()
	p.Serve = serve
	p.KeepAwake = true
	return p
}

func wantAwake(t *testing.T, f *FakeSleepInhibitor, g *Governor, reason string) {
	t.Helper()
	held, got := f.Held()
	if reason == "" {
		if held {
			t.Fatalf("inhibitor held (%q), want released", got)
		}
	} else if !held || got != reason {
		t.Fatalf("inhibitor held=%v reason=%q, want held with %q", held, got, reason)
	}
	if g.AwakeReason() != reason {
		t.Fatalf("AwakeReason() = %q, want %q", g.AwakeReason(), reason)
	}
}

// An unenrolled daemon (standalone, or not yet logged in) stays awake only
// for the request it is answering.
func TestAwakeFollowsInflightWhenUnenrolled(t *testing.T) {
	h := newHarness(t, awakePolicy("always"))
	f := &FakeSleepInhibitor{}
	h.g.SetSleepInhibitor(f)
	wantAwake(t, f, h.g, "") // serving, but nobody is placing work here

	_, rel1, err := h.g.Admit(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	wantAwake(t, f, h.g, AwakeReasonRequest)
	_, rel2, err := h.g.Admit(context.Background(), "r2")
	if err != nil {
		t.Fatal(err)
	}
	rel1()
	wantAwake(t, f, h.g, AwakeReasonRequest) // r2 still running
	rel2()
	rel2() // release is idempotent
	wantAwake(t, f, h.g, "")
	holds, releases := f.Counts()
	if len(holds) != 1 || releases != 1 {
		t.Fatalf("holds=%v releases=%d, want one of each", holds, releases)
	}
}

// Enrolled and serving holds; yield, pause and leaving the schedule
// release; coming back re-takes it.
func TestAwakeFollowsServingWhenEnrolled(t *testing.T) {
	h := newHarness(t, awakePolicy("idle-only"))
	f := &FakeSleepInhibitor{}
	h.g.SetSleepInhibitor(f)
	h.g.SetEnrolled(true)
	wantAwake(t, f, h.g, "") // idle-only starts yielded

	h.idle.Set(5 * time.Minute)
	h.g.evaluate(context.Background())
	wantAwake(t, f, h.g, AwakeReasonServing)

	h.idle.Set(0) // operator back: yield
	h.g.evaluate(context.Background())
	wantAwake(t, f, h.g, "")

	h.idle.Set(5 * time.Minute)
	h.g.evaluate(context.Background())
	wantAwake(t, f, h.g, AwakeReasonServing)

	h.power.Set(PowerStatus{OnBattery: true}) // serve_on_battery=false
	h.g.evaluate(context.Background())
	if h.g.State() != StatePausedBattery {
		t.Fatalf("state = %s", h.g.State())
	}
	wantAwake(t, f, h.g, "")

	h.power.Set(PowerStatus{})
	h.g.evaluate(context.Background())
	wantAwake(t, f, h.g, AwakeReasonServing)

	h.g.SetEnrolled(false) // e.g. credentials gone
	wantAwake(t, f, h.g, "")
}

// On yield the assertion outlives the state for as long as the in-flight
// request does (at most YieldGrace, then the governor cancels it), and
// the reason shown to the OS follows what is actually keeping it.
func TestAwakeReasonTracksRequestsAcrossYield(t *testing.T) {
	h := newHarness(t, awakePolicy("idle-only"))
	f := &FakeSleepInhibitor{}
	h.g.SetSleepInhibitor(f)
	h.g.SetEnrolled(true)
	h.idle.Set(5 * time.Minute)
	h.g.evaluate(context.Background())
	wantAwake(t, f, h.g, AwakeReasonServing)

	ctx, release, err := h.g.Admit(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	wantAwake(t, f, h.g, AwakeReasonRequest)

	h.idle.Set(0)
	h.g.evaluate(context.Background())
	wantAwake(t, f, h.g, AwakeReasonRequest) // draining
	if !h.clk.BlockUntilWaiters(1, 2*time.Second) {
		t.Fatal("drain goroutine never armed its grace timer")
	}
	h.clk.Advance(3 * time.Second) // past YieldGrace
	eventually(t, func() bool { return ctx.Err() != nil }, "request not cancelled after grace")
	wantAwake(t, f, h.g, AwakeReasonRequest) // cancelled, not yet released by its handler
	release()
	wantAwake(t, f, h.g, "")
}

func TestAwakeDisabledByPolicy(t *testing.T) {
	p := awakePolicy("always")
	p.KeepAwake = false
	h := newHarness(t, p)
	f := &FakeSleepInhibitor{}
	h.g.SetSleepInhibitor(f)
	h.g.SetEnrolled(true)
	_, release, err := h.g.Admit(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	wantAwake(t, f, h.g, "")
	release()

	// Switched on live (SetPolicy): takes effect at once, and off again
	// releases at once.
	p.KeepAwake = true
	h.g.SetPolicy(p)
	wantAwake(t, f, h.g, AwakeReasonServing)
	p.KeepAwake = false
	h.g.SetPolicy(p)
	wantAwake(t, f, h.g, "")
}

// Stop/exit release for good: nothing after Close re-takes the assertion.
func TestAwakeCloseReleasesAndStays(t *testing.T) {
	h := newHarness(t, awakePolicy("always"))
	f := &FakeSleepInhibitor{}
	h.g.SetSleepInhibitor(f)
	h.g.SetEnrolled(true)
	wantAwake(t, f, h.g, AwakeReasonServing)
	h.g.Close()
	wantAwake(t, f, h.g, "")
	if _, _, err := h.g.Admit(context.Background(), "late"); err != nil {
		t.Fatal(err)
	}
	wantAwake(t, f, h.g, "")
	h.g.Close() // idempotent
}

// A platform that cannot hold (stub, missing helper) is logged once and
// never fatal; it is retried on the next transition.
func TestAwakeHoldFailureIsSoft(t *testing.T) {
	h := newHarness(t, awakePolicy("always"))
	f := &FakeSleepInhibitor{}
	f.Fail(errors.New("no caffeinate"))
	h.g.SetSleepInhibitor(f)
	h.g.SetEnrolled(true)
	wantAwake(t, f, h.g, "")
	if _, _, err := h.g.Admit(context.Background(), "r1"); err != nil {
		t.Fatal(err)
	}
	wantAwake(t, f, h.g, "")
	f.Fail(nil)
	h.g.SetEnrolled(true) // any sync
	wantAwake(t, f, h.g, AwakeReasonRequest)
}

// No inhibitor installed (tests elsewhere, platforms without one): the
// governor is unchanged.
func TestAwakeWithoutInhibitor(t *testing.T) {
	h := newHarness(t, awakePolicy("always"))
	h.g.SetEnrolled(true)
	if _, release, err := h.g.Admit(context.Background(), "r1"); err != nil {
		t.Fatal(err)
	} else {
		release()
	}
	if h.g.AwakeReason() != "" {
		t.Fatal("reason without inhibitor")
	}
	h.g.Close()
}
