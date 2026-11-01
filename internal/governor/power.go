package governor

import (
	"errors"
	"sync"
)

// SleepInhibitor keeps the operating system from going to sleep on its
// own while held. It is the one place flockd touches power management,
// and it is deliberately narrow: an inhibitor stops the *automatic idle*
// sleep timer only. It never keeps the display on, never blocks a sleep
// the operator asks for (lid, menu, power button), and never wakes the
// machine. Platform implementations live in power_*.go; the Governor
// decides when to hold it (see Governor.syncAwake) and tests use
// FakeSleepInhibitor.
type SleepInhibitor interface {
	// Hold takes the assertion, tagged with a reason the operator can read
	// in the OS's own tooling (powercfg /requests, pmset -g assertions,
	// systemd-inhibit --list). Holding an already-held inhibitor is a
	// no-op; the first reason stays.
	Hold(reason string) error
	// Release drops the assertion. Releasing an unheld inhibitor is a
	// no-op.
	Release() error
}

// Reasons the governor holds the inhibitor for, as the OS shows them.
const (
	AwakeReasonRequest = "Teraflock: inference request in flight"
	AwakeReasonServing = "Teraflock: serving the mesh"
)

// ErrNoSleepInhibitor is what the stub platform inhibitor returns from
// Hold: the governor logs it once and carries on without an assertion.
var ErrNoSleepInhibitor = errors.New("governor: sleep inhibitor not implemented on this platform")

// SetSleepInhibitor installs the platform inhibitor (nil = none: the OS
// sleeps whenever it likes, which is the pre-v0.7 behaviour). Call it
// before Run; the first sync happens on the next transition.
func (g *Governor) SetSleepInhibitor(i SleepInhibitor) {
	g.awakeMu.Lock()
	g.inhibitor = i
	g.awakeMu.Unlock()
	g.syncAwake()
}

// SetEnrolled tells the governor whether the node is part of a mesh. An
// enrolled node that is serving stays awake so placements can reach it
// ("serve the mesh while you sleep" means the operator sleeps, not the
// machine); an unenrolled or standalone daemon only stays awake for the
// request it is answering right now.
func (g *Governor) SetEnrolled(enrolled bool) {
	g.mu.Lock()
	g.enrolled = enrolled
	g.mu.Unlock()
	g.syncAwake()
}

// Close releases the sleep assertion for good: later syncs are no-ops.
// Called on daemon exit; stop/exit must never leave the machine insomniac.
func (g *Governor) Close() {
	g.awakeMu.Lock()
	defer g.awakeMu.Unlock()
	g.awakeClosed = true
	g.releaseLocked()
}

// AwakeReason reports why the sleep assertion is held ("" when it is
// not), for status output and tests.
func (g *Governor) AwakeReason() string {
	g.awakeMu.Lock()
	defer g.awakeMu.Unlock()
	return g.awakeReason
}

// wantAwake is the sleep policy (docs/sleep.md) in one place: hold while a
// request is in flight (whatever the state — a yield drains for at most
// YieldGrace, and a request the OS suspends mid-stream is a failed
// request the coordinator has to retry), or while serving and enrolled.
// Yielded, paused (battery, thermal), outside the schedule, unenrolled
// and idle all release. keep_awake = false switches the whole thing off.
func (g *Governor) wantAwake() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.policy.KeepAwake {
		return ""
	}
	if len(g.inflight) > 0 {
		return AwakeReasonRequest
	}
	if g.state == StateServing && g.enrolled {
		return AwakeReasonServing
	}
	return ""
}

// syncAwake brings the inhibitor in line with wantAwake. It runs after
// every event that can change the answer (Admit, release, evaluate,
// SetPolicy, SetEnrolled) and never under g.mu: the platform Hold may
// spawn a process. awakeMu serialises the OS calls so two syncs cannot
// cross and leave the assertion in the wrong state.
func (g *Governor) syncAwake() {
	g.awakeMu.Lock()
	defer g.awakeMu.Unlock()
	if g.inhibitor == nil || g.awakeClosed {
		return
	}
	want := g.wantAwake()
	switch {
	case want == "" && g.awakeReason != "":
		g.releaseLocked()
	case want != "" && g.awakeReason == "":
		if err := g.inhibitor.Hold(want); err != nil {
			if !g.warnedAwake {
				g.log.Warn("keep-awake unavailable; the OS may sleep while serving", "err", err)
				g.warnedAwake = true
			}
			return
		}
		g.awakeReason = want
		g.log.Info("keep-awake held", "reason", want)
	case want != "" && want != g.awakeReason:
		// The reason changed (a request finished on an enrolled serving
		// node, or arrived on an unenrolled one). The OS tooling shows the
		// reason, so re-take it rather than show a stale one.
		g.releaseLocked()
		if err := g.inhibitor.Hold(want); err != nil {
			g.log.Warn("keep-awake re-take failed", "err", err)
			return
		}
		g.awakeReason = want
	}
}

func (g *Governor) releaseLocked() {
	if g.inhibitor == nil || g.awakeReason == "" {
		return
	}
	if err := g.inhibitor.Release(); err != nil {
		g.log.Warn("keep-awake release failed", "err", err)
	} else {
		g.log.Info("keep-awake released", "was", g.awakeReason)
	}
	g.awakeReason = ""
}

// FakeSleepInhibitor records holds and releases for tests (here and in
// packages that build a governor with one).
type FakeSleepInhibitor struct {
	mu      sync.Mutex
	held    bool
	reason  string
	holds   []string
	release int
	fail    error
}

// Held reports whether the fake is held and with which reason.
func (f *FakeSleepInhibitor) Held() (bool, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.held, f.reason
}

// Counts reports every reason Hold was called with, in order, and how
// many times Release was.
func (f *FakeSleepInhibitor) Counts() (holds []string, releases int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.holds...), f.release
}

// Fail makes the next Holds return err (nil clears it).
func (f *FakeSleepInhibitor) Fail(err error) {
	f.mu.Lock()
	f.fail = err
	f.mu.Unlock()
}

func (f *FakeSleepInhibitor) Hold(reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	if f.held {
		return nil
	}
	f.held, f.reason = true, reason
	f.holds = append(f.holds, reason)
	return nil
}

func (f *FakeSleepInhibitor) Release() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.held {
		return nil
	}
	f.held, f.reason = false, ""
	f.release++
	return nil
}
