// Package governor is the resource-governance policy engine (SPEC §4.1,
// §10). It consumes idle/power signals, enforces the operator's serve
// policy, and guarantees instant-yield: on operator activity, in-flight
// requests are drained-or-cancelled within the configured grace window and
// the node marks itself YIELDED.
package governor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/hivegrid/hived/internal/clock"
	typesv1 "github.com/hivegrid/proto/gen/go/hive/types/v1"
)

// State is the governor's serving decision.
type State int

const (
	// StateServing: the node accepts and runs inference.
	StateServing State = iota
	// StateYielded: the operator is using the machine (idle-only policy).
	StateYielded
	// StatePausedBattery: on battery and serve_on_battery=false.
	StatePausedBattery
	// StatePausedThermal: above the temperature ceiling.
	StatePausedThermal
	// StateOutsideSchedule: serve_policy=scheduled and now is out of window.
	StateOutsideSchedule
)

func (s State) String() string {
	switch s {
	case StateServing:
		return "serving"
	case StateYielded:
		return "yielded"
	case StatePausedBattery:
		return "paused-battery"
	case StatePausedThermal:
		return "paused-thermal"
	case StateOutsideSchedule:
		return "outside-schedule"
	default:
		return "unknown"
	}
}

// NodeState maps to the tunnel heartbeat enum.
func (s State) NodeState() typesv1.NodeState {
	if s == StateServing {
		return typesv1.NodeState_NODE_STATE_READY
	}
	return typesv1.NodeState_NODE_STATE_YIELDED
}

// ErrNotServing is returned by Admit when the governor refuses work.
type ErrNotServing struct{ State State }

func (e ErrNotServing) Error() string {
	return fmt.Sprintf("governor: not serving (state=%s)", e.State)
}

// IdleSource reports how long user input has been quiet. Platform
// implementations live in idle_*.go; tests use FakeIdleSource.
type IdleSource interface {
	IdleFor(ctx context.Context) (time.Duration, error)
}

// PowerStatus is a battery/thermal snapshot. TempCelsius == 0 means unknown.
type PowerStatus struct {
	OnBattery   bool
	TempCelsius float64
}

// PowerSource reports battery/thermal state.
type PowerSource interface {
	Status(ctx context.Context) (PowerStatus, error)
}

// Policy is the operator's resource-governance configuration.
type Policy struct {
	Serve          string // always|idle-only|scheduled
	IdleAfter      time.Duration
	YieldGrace     time.Duration // drain-or-cancel window, default 2s
	PollInterval   time.Duration
	ServeOnBattery bool
	MaxTempCelsius float64
	Schedule       []Window
}

// Governor evaluates Policy against live signals.
type Governor struct {
	policy Policy
	idle   IdleSource
	power  PowerSource
	clock  clock.Clock
	log    *slog.Logger

	mu           sync.Mutex
	state        State
	inflight     map[string]context.CancelFunc
	drainWaiters []chan struct{}
	subs         []chan State
	lastPower    PowerStatus
	warnedIdle   bool
}

// New builds a Governor. clk may be nil (wall clock).
func New(p Policy, idle IdleSource, power PowerSource, clk clock.Clock, log *slog.Logger) *Governor {
	if clk == nil {
		clk = clock.Real{}
	}
	if log == nil {
		log = slog.Default()
	}
	if p.YieldGrace <= 0 {
		p.YieldGrace = 2 * time.Second
	}
	if p.PollInterval <= 0 {
		p.PollInterval = 2 * time.Second
	}
	g := &Governor{
		policy:   p,
		idle:     idle,
		power:    power,
		clock:    clk,
		log:      log,
		inflight: make(map[string]context.CancelFunc),
	}
	// Start pessimistic for idle-only (assume operator active until proven
	// idle); optimistic otherwise.
	if p.Serve == "idle-only" {
		g.state = StateYielded
	} else {
		g.state = StateServing
	}
	return g
}

// Run polls signals until ctx ends.
func (g *Governor) Run(ctx context.Context) {
	for {
		g.mu.Lock()
		interval := g.policy.PollInterval
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-g.clock.After(interval):
			g.evaluate(ctx)
		}
	}
}

// SetPolicy swaps the policy at runtime (`hive limits` / PUT /api/v1/limits).
// The next evaluate() applies it.
func (g *Governor) SetPolicy(p Policy) {
	if p.YieldGrace <= 0 {
		p.YieldGrace = 2 * time.Second
	}
	if p.PollInterval <= 0 {
		p.PollInterval = 2 * time.Second
	}
	g.mu.Lock()
	g.policy = p
	g.mu.Unlock()
}

// Policy returns a copy of the active policy.
func (g *Governor) Policy() Policy {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.policy
}

// State returns the current decision.
func (g *Governor) State() State {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.state
}

// Power returns the last observed power status (for heartbeats).
func (g *Governor) Power() PowerStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lastPower
}

// Inflight returns the number of admitted, unfinished requests.
func (g *Governor) Inflight() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.inflight)
}

// Subscribe returns a channel receiving state transitions (best-effort;
// slow consumers may miss intermediate states but always converge).
func (g *Governor) Subscribe() <-chan State {
	g.mu.Lock()
	defer g.mu.Unlock()
	ch := make(chan State, 16)
	g.subs = append(g.subs, ch)
	return ch
}

// Admit gates one request. On success it returns a request context that the
// governor cancels if the yield grace expires, plus a release func the
// caller MUST invoke when the request finishes (success or not).
func (g *Governor) Admit(parent context.Context, id string) (context.Context, func(), error) {
	g.mu.Lock()
	if g.state != StateServing {
		st := g.state
		g.mu.Unlock()
		return nil, nil, ErrNotServing{State: st}
	}
	ctx, cancel := context.WithCancel(parent)
	g.inflight[id] = cancel
	g.mu.Unlock()

	var once sync.Once
	release := func() {
		once.Do(func() {
			g.mu.Lock()
			delete(g.inflight, id)
			if len(g.inflight) == 0 {
				for _, w := range g.drainWaiters {
					close(w)
				}
				g.drainWaiters = nil
			}
			g.mu.Unlock()
			cancel()
		})
	}
	return ctx, release, nil
}

// evaluate samples signals and applies transitions. Exported behavior is
// tested by driving this directly with fakes (see governor_test.go).
func (g *Governor) evaluate(ctx context.Context) {
	g.mu.Lock()
	pol := g.policy
	g.mu.Unlock()
	desired := g.desiredState(ctx, pol)

	g.mu.Lock()
	prev := g.state
	g.state = desired
	subs := append([]chan State(nil), g.subs...)
	g.mu.Unlock()

	if desired == prev {
		return
	}
	g.log.Info("governor state change", "from", prev.String(), "to", desired.String())
	for _, ch := range subs {
		select {
		case ch <- desired:
		default:
		}
	}
	if prev == StateServing {
		// Instant-yield: give in-flight requests YieldGrace to drain, then
		// cancel whatever is left (SPEC §4.1).
		go g.drainOrCancel()
	}
}

func (g *Governor) desiredState(ctx context.Context, pol Policy) State {
	// Battery/thermal guards apply under every policy.
	if g.power != nil {
		ps, err := g.power.Status(ctx)
		if err == nil {
			g.mu.Lock()
			g.lastPower = ps
			g.mu.Unlock()
			if ps.OnBattery && !pol.ServeOnBattery {
				return StatePausedBattery
			}
			if pol.MaxTempCelsius > 0 && ps.TempCelsius > pol.MaxTempCelsius {
				return StatePausedThermal
			}
		} else {
			g.log.Debug("power probe failed", "err", err)
		}
	}

	switch pol.Serve {
	case "always":
		return StateServing
	case "scheduled":
		if !inAnyWindow(pol.Schedule, g.clock.Now()) {
			return StateOutsideSchedule
		}
		return StateServing
	default: // idle-only
		idleFor, err := g.idle.IdleFor(ctx)
		if err != nil {
			// Unknown idle state (headless box, stub platform): assume idle
			// but warn once — serving must not silently break on servers.
			if !g.warnedIdle {
				g.log.Warn("idle detection unavailable; assuming idle", "err", err)
				g.warnedIdle = true
			}
			return StateServing
		}
		if idleFor < pol.IdleAfter {
			return StateYielded
		}
		return StateServing
	}
}

// drainOrCancel waits up to YieldGrace for in-flight work, then cancels.
func (g *Governor) drainOrCancel() {
	g.mu.Lock()
	grace := g.policy.YieldGrace
	g.mu.Unlock()
	select {
	case <-g.drainedChan():
		return // drained cleanly within grace
	case <-g.clock.After(grace):
	}
	g.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(g.inflight))
	ids := make([]string, 0, len(g.inflight))
	for id, c := range g.inflight {
		cancels = append(cancels, c)
		ids = append(ids, id)
	}
	g.mu.Unlock()
	if len(cancels) > 0 {
		g.log.Info("yield grace expired; cancelling in-flight requests", "count", len(cancels), "ids", ids)
	}
	for _, c := range cancels {
		c()
	}
}

func (g *Governor) drainedChan() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	ch := make(chan struct{})
	if len(g.inflight) == 0 {
		close(ch)
		return ch
	}
	g.drainWaiters = append(g.drainWaiters, ch)
	return ch
}

// FakeIdleSource / FakePowerSource are shared by tests here and in other
// packages (localapi, tunnel) that need a controllable governor.
type FakeIdleSource struct {
	mu   sync.Mutex
	dur  time.Duration
	fail error
}

func (f *FakeIdleSource) Set(d time.Duration) {
	f.mu.Lock()
	f.dur = d
	f.mu.Unlock()
}

func (f *FakeIdleSource) Fail(err error) {
	f.mu.Lock()
	f.fail = err
	f.mu.Unlock()
}

func (f *FakeIdleSource) IdleFor(context.Context) (time.Duration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return 0, f.fail
	}
	return f.dur, nil
}

type FakePowerSource struct {
	mu sync.Mutex
	ps PowerStatus
}

func (f *FakePowerSource) Set(ps PowerStatus) {
	f.mu.Lock()
	f.ps = ps
	f.mu.Unlock()
}

func (f *FakePowerSource) Status(context.Context) (PowerStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ps, nil
}

// ErrNoIdleSource is returned by stub platform sources.
var ErrNoIdleSource = errors.New("governor: idle detection not implemented on this platform")
