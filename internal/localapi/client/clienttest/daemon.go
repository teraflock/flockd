// Package clienttest stands up a real localapi.Server on an httptest
// listener — the generated router, the mock runtime, a governor with fake
// signal sources, the event hub and the log ring — so client, CLI, TUI and
// `tera mcp` tests talk to the actual API contract rather than a
// hand-rolled fake that could drift from it.
package clienttest

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/teraflock/flockd/internal/engine"
	"github.com/teraflock/flockd/internal/events"
	"github.com/teraflock/flockd/internal/governor"
	"github.com/teraflock/flockd/internal/localapi"
	"github.com/teraflock/flockd/internal/logging"
	rt "github.com/teraflock/flockd/internal/runtime"
	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
)

// Token is the bearer token the fake daemon accepts.
const Token = "flock_testtoken"

// Model is the mock model the daemon serves.
const Model = "mock-8b-instruct"

// Options tunes the fake daemon.
type Options struct {
	// ServePolicy is the governor policy: "always" (default) serves;
	// "idle-only" with the fake idle source at 0 refuses inference with
	// 503, the way a node does while the operator is at the keyboard.
	ServePolicy string
	// TokensPerSec throttles the mock runtime (0 = unthrottled).
	TokensPerSec float64
	// ReasoningTokens makes chat emit chain-of-thought deltas first.
	ReasoningTokens int
}

// Daemon is the running fake: URL and Token feed client.Options; the
// sources let a test flip the governor mid-run.
type Daemon struct {
	*httptest.Server
	Token    string
	Governor *governor.Governor
	Idle     *governor.FakeIdleSource
	Power    *governor.FakePowerSource
	Events   *events.Hub
	// Log is the daemon's logger; lines logged through it land in LogRing
	// (GET /api/v1/logs, `log` events) the way the real daemon's do.
	Log     *slog.Logger
	LogRing *logging.Ring
	Engine  *engine.Engine
}

// New starts a daemon and stops it when the test ends.
func New(t testing.TB, o Options) *Daemon {
	t.Helper()
	if o.ServePolicy == "" {
		o.ServePolicy = "always"
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	idle := &governor.FakeIdleSource{}
	idle.Set(time.Hour)
	if o.ServePolicy == "idle-only" {
		idle.Set(0) // active operator => yielded
	}
	power := &governor.FakePowerSource{}
	gov := governor.New(governor.Policy{Serve: o.ServePolicy, IdleAfter: time.Minute}, idle, power, nil, quiet)

	eng := engine.New(gov, nil, nil)
	mock := rt.NewMockRuntime(o.TokensPerSec)
	mock.ReasoningTokens = o.ReasoningTokens
	inst, err := mock.Load(context.Background(), rt.ModelSpec{ID: Model}, rt.ResourceBudget{MaxConcurrent: 4})
	if err != nil {
		t.Fatal(err)
	}
	eng.Register(rt.ModelSpec{ID: Model}, inst)

	hub := events.NewHub()
	log, ring := logging.New("info", "text")
	s := localapi.New(localapi.Deps{
		Engine:     eng,
		Governor:   gov,
		Events:     hub,
		LogRing:    ring,
		Hardware:   &typesv1.CapabilityProfile{Os: "darwin", Arch: "arm64", CpuCores: 8, RamTotalMb: 32768},
		Log:        log,
		NodeID:     "node-test",
		Version:    "test",
		Standalone: true,
		Token:      Token,
	})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &Daemon{Server: srv, Token: Token, Governor: gov, Idle: idle, Power: power, Events: hub, Log: log, LogRing: ring, Engine: eng}
}
