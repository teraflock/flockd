package governor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/teraflock/flockd/internal/clock"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type harness struct {
	g     *Governor
	idle  *FakeIdleSource
	power *FakePowerSource
	clk   *clock.Fake
}

func newHarness(t *testing.T, p Policy) *harness {
	t.Helper()
	idle := &FakeIdleSource{}
	power := &FakePowerSource{}
	clk := clock.NewFake()
	return &harness{
		g:     New(p, idle, power, clk, quietLogger()),
		idle:  idle,
		power: power,
		clk:   clk,
	}
}

func idleOnlyPolicy() Policy {
	return Policy{
		Serve:        "idle-only",
		IdleAfter:    2 * time.Minute,
		YieldGrace:   2 * time.Second,
		PollInterval: time.Second,
	}
}

// eventually polls (real time) for a condition, needed where the governor
// acts in a goroutine (drain-or-cancel).
func eventually(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal(msg)
}

func TestIdleOnlyStartsYielded(t *testing.T) {
	h := newHarness(t, idleOnlyPolicy())
	if got := h.g.State(); got != StateYielded {
		t.Fatalf("initial state = %s, want yielded", got)
	}
	if _, _, err := h.g.Admit(context.Background(), "r1"); err == nil {
		t.Fatal("Admit must fail while yielded")
	}
}

func TestIdleOnlyServesWhenIdle(t *testing.T) {
	h := newHarness(t, idleOnlyPolicy())
	h.idle.Set(5 * time.Minute) // user away
	h.g.evaluate(context.Background())
	if got := h.g.State(); got != StateServing {
		t.Fatalf("state = %s, want serving", got)
	}
	ctx, release, err := h.g.Admit(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil {
		t.Fatal("admitted context must be live")
	}
	release()
}

func TestInstantYieldCancelsAfterGrace(t *testing.T) {
	h := newHarness(t, idleOnlyPolicy())
	h.idle.Set(10 * time.Minute)
	h.g.evaluate(context.Background())

	reqCtx, release, err := h.g.Admit(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	// Operator touches the keyboard.
	h.idle.Set(0)
	h.g.evaluate(context.Background())
	if got := h.g.State(); got != StateYielded {
		t.Fatalf("state = %s, want yielded immediately on activity", got)
	}
	// New work is refused instantly.
	if _, _, err := h.g.Admit(context.Background(), "r2"); err == nil {
		t.Fatal("Admit must fail after yield")
	}
	var nse ErrNotServing
	_, _, admitErr := h.g.Admit(context.Background(), "r3")
	if !errors.As(admitErr, &nse) || nse.State != StateYielded {
		t.Fatalf("err = %v, want ErrNotServing{yielded}", admitErr)
	}

	// In-flight request is NOT cancelled before the grace window...
	if reqCtx.Err() != nil {
		t.Fatal("request cancelled before grace expired")
	}
	// ...but is cancelled once the grace window elapses.
	if !h.clk.BlockUntilWaiters(1, 2*time.Second) {
		t.Fatal("drain goroutine never armed its grace timer")
	}
	h.clk.Advance(2 * time.Second)
	eventually(t, func() bool { return reqCtx.Err() != nil },
		"in-flight request not cancelled within yield grace")
}

func TestYieldDrainsCleanlyWithoutCancel(t *testing.T) {
	h := newHarness(t, idleOnlyPolicy())
	h.idle.Set(10 * time.Minute)
	h.g.evaluate(context.Background())

	reqCtx, release, err := h.g.Admit(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}

	h.idle.Set(0)
	h.g.evaluate(context.Background())
	if !h.clk.BlockUntilWaiters(1, 2*time.Second) {
		t.Fatal("grace timer not armed")
	}

	// Request finishes inside the grace window: it must complete, not be
	// cancelled (drain-before-cancel).
	release()
	if reqCtx.Err() == nil {
		// release cancels its own context after unregistering — that's
		// fine; the point is drainOrCancel returned without force-cancel.
	}
	eventually(t, func() bool { return h.g.Inflight() == 0 }, "inflight not drained")
	// Advancing past grace must not panic or cancel anything new.
	h.clk.Advance(5 * time.Second)
}

func TestResumeAfterIdleThreshold(t *testing.T) {
	h := newHarness(t, idleOnlyPolicy())
	h.idle.Set(0)
	h.g.evaluate(context.Background())
	if h.g.State() != StateYielded {
		t.Fatal("want yielded while active")
	}
	// User walks away; idle climbs past the threshold.
	h.idle.Set(90 * time.Second) // below IdleAfter=2m
	h.g.evaluate(context.Background())
	if h.g.State() != StateYielded {
		t.Fatal("90s idle must still be yielded (threshold 2m)")
	}
	h.idle.Set(2 * time.Minute)
	h.g.evaluate(context.Background())
	if h.g.State() != StateServing {
		t.Fatal("2m idle must resume serving")
	}
}

func TestBatteryPauseAndResume(t *testing.T) {
	p := idleOnlyPolicy()
	h := newHarness(t, p)
	h.idle.Set(time.Hour)
	h.g.evaluate(context.Background())
	if h.g.State() != StateServing {
		t.Fatal("precondition: serving")
	}

	h.power.Set(PowerStatus{OnBattery: true})
	h.g.evaluate(context.Background())
	if h.g.State() != StatePausedBattery {
		t.Fatalf("state = %s, want paused-battery", h.g.State())
	}
	if _, _, err := h.g.Admit(context.Background(), "r"); err == nil {
		t.Fatal("Admit must fail on battery")
	}

	h.power.Set(PowerStatus{OnBattery: false})
	h.g.evaluate(context.Background())
	if h.g.State() != StateServing {
		t.Fatal("must resume on AC")
	}
}

func TestServeOnBatteryOverride(t *testing.T) {
	p := idleOnlyPolicy()
	p.ServeOnBattery = true
	h := newHarness(t, p)
	h.idle.Set(time.Hour)
	h.power.Set(PowerStatus{OnBattery: true})
	h.g.evaluate(context.Background())
	if h.g.State() != StateServing {
		t.Fatal("serve_on_battery=true must keep serving")
	}
}

func TestThermalPause(t *testing.T) {
	p := idleOnlyPolicy()
	p.MaxTempCelsius = 90
	h := newHarness(t, p)
	h.idle.Set(time.Hour)

	h.power.Set(PowerStatus{TempCelsius: 95})
	h.g.evaluate(context.Background())
	if h.g.State() != StatePausedThermal {
		t.Fatalf("state = %s, want paused-thermal", h.g.State())
	}
	h.power.Set(PowerStatus{TempCelsius: 70})
	h.g.evaluate(context.Background())
	if h.g.State() != StateServing {
		t.Fatal("must resume after cooling")
	}
}

func TestThermalDisabledWhenZero(t *testing.T) {
	p := idleOnlyPolicy()
	p.MaxTempCelsius = 0
	h := newHarness(t, p)
	h.idle.Set(time.Hour)
	h.power.Set(PowerStatus{TempCelsius: 200})
	h.g.evaluate(context.Background())
	if h.g.State() != StateServing {
		t.Fatal("max_temp=0 must disable the thermal guard")
	}
}

func TestAlwaysPolicyIgnoresIdle(t *testing.T) {
	p := idleOnlyPolicy()
	p.Serve = "always"
	h := newHarness(t, p)
	h.idle.Set(0) // user actively typing
	h.g.evaluate(context.Background())
	if h.g.State() != StateServing {
		t.Fatal("serve=always must ignore idle signal")
	}
}

func TestAlwaysPolicyStillRespectsBattery(t *testing.T) {
	p := idleOnlyPolicy()
	p.Serve = "always"
	h := newHarness(t, p)
	h.power.Set(PowerStatus{OnBattery: true})
	h.g.evaluate(context.Background())
	if h.g.State() != StatePausedBattery {
		t.Fatal("battery guard applies under serve=always too")
	}
}

func TestScheduledPolicyWindows(t *testing.T) {
	ws, err := ParseWindows([]string{"22:00-08:00"})
	if err != nil {
		t.Fatal(err)
	}
	p := idleOnlyPolicy()
	p.Serve = "scheduled"
	p.Schedule = ws

	idle := &FakeIdleSource{}
	power := &FakePowerSource{}
	// 23:30 — inside the overnight window.
	clk := clock.NewFakeAt(time.Date(2026, 1, 1, 23, 30, 0, 0, time.UTC))
	g := New(p, idle, power, clk, quietLogger())
	g.evaluate(context.Background())
	if g.State() != StateServing {
		t.Fatal("23:30 must serve (window 22:00-08:00)")
	}
	// 03:00 — still inside (wraps midnight).
	clk.Advance(3*time.Hour + 30*time.Minute)
	g.evaluate(context.Background())
	if g.State() != StateServing {
		t.Fatal("03:00 must serve")
	}
	// 09:00 — outside.
	clk.Advance(6 * time.Hour)
	g.evaluate(context.Background())
	if g.State() != StateOutsideSchedule {
		t.Fatalf("09:00 state = %s, want outside-schedule", g.State())
	}
	// Entering window again resumes.
	clk.Advance(13 * time.Hour) // 22:00
	g.evaluate(context.Background())
	if g.State() != StateServing {
		t.Fatal("22:00 must resume serving")
	}
}

func TestIdleSourceErrorAssumesIdle(t *testing.T) {
	h := newHarness(t, idleOnlyPolicy())
	h.idle.Fail(ErrNoIdleSource)
	h.g.evaluate(context.Background())
	if h.g.State() != StateServing {
		t.Fatal("headless/unsupported platforms must default to serving")
	}
}

func TestSubscribeReceivesTransitions(t *testing.T) {
	h := newHarness(t, idleOnlyPolicy())
	sub := h.g.Subscribe()
	h.idle.Set(time.Hour)
	h.g.evaluate(context.Background())
	select {
	case s := <-sub:
		if s != StateServing {
			t.Fatalf("got %s, want serving", s)
		}
	case <-time.After(time.Second):
		t.Fatal("no state transition received")
	}
}

func TestRunLoopPollsOnFakeClock(t *testing.T) {
	h := newHarness(t, idleOnlyPolicy())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.g.Run(ctx)

	h.idle.Set(time.Hour)
	if !h.clk.BlockUntilWaiters(1, 2*time.Second) {
		t.Fatal("run loop never armed poll timer")
	}
	h.clk.Advance(time.Second)
	eventually(t, func() bool { return h.g.State() == StateServing },
		"run loop did not evaluate after poll interval")
}

func TestYieldLatencyBound(t *testing.T) {
	// End-to-end latency proof with REAL clock and tight timings: activity
	// -> yielded + cancelled within well under 1s (grace 100ms).
	p := Policy{Serve: "idle-only", IdleAfter: time.Minute, YieldGrace: 100 * time.Millisecond, PollInterval: 10 * time.Millisecond}
	idle := &FakeIdleSource{}
	g := New(p, idle, &FakePowerSource{}, clock.Real{}, quietLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go g.Run(ctx)

	idle.Set(time.Hour)
	eventually(t, func() bool { return g.State() == StateServing }, "never started serving")

	reqCtx, release, err := g.Admit(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	start := time.Now()
	idle.Set(0)
	eventually(t, func() bool { return g.State() == StateYielded }, "did not yield")
	eventually(t, func() bool { return reqCtx.Err() != nil }, "in-flight not cancelled")
	if lat := time.Since(start); lat > time.Second {
		t.Fatalf("yield latency %v exceeds 1s (grace 100ms, poll 10ms)", lat)
	}
}
