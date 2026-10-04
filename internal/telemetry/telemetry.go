// Package telemetry assembles heartbeats and keeps rolling performance
// stats (tok/s, request counters) shared by the tunnel, local API and TUI.
package telemetry

import (
	"sync"
	"time"

	tunnelv1 "github.com/teraflock/proto/gen/go/flock/tunnel/v1"
	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Stats aggregates node-local counters. Safe for concurrent use.
type Stats struct {
	mu sync.Mutex

	now func() time.Time // injectable for tests

	tokens   ring // token completion events
	requests ring // request completion events

	// Session counters: since this process started.
	totalRequests   int64
	totalTokens     int64
	inflight        int
	earnedMicrocred int64 // standalone/demo earnings counter (micro-credits)

	// base is what earlier runs of the daemon counted (Restore): lifetime
	// = base + session. since is when lifetime counting began.
	base    Counters
	started time.Time
}

// Counters are the lifetime totals that survive a daemon restart
// (persist.go). Numbers only: nothing about any request.
type Counters struct {
	Tokens          int64     `json:"total_tokens"`
	Requests        int64     `json:"total_requests"`
	EarnedMicrocred int64     `json:"earned_microcredits"`
	Since           time.Time `json:"since"`
}

// Restore sets the totals of earlier runs; lifetime figures continue from
// them. Call before serving.
func (s *Stats) Restore(c Counters) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.base = c
}

// Lifetime returns the totals to persist: earlier runs plus this one.
func (s *Stats) Lifetime() Counters {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lifetimeLocked()
}

func (s *Stats) lifetimeLocked() Counters {
	c := Counters{
		Tokens:          s.base.Tokens + s.totalTokens,
		Requests:        s.base.Requests + s.totalRequests,
		EarnedMicrocred: s.base.EarnedMicrocred + s.earnedMicrocred,
		Since:           s.base.Since,
	}
	if c.Since.IsZero() {
		c.Since = s.started
	}
	return c
}

// NewStats returns a Stats using the real clock.
func NewStats() *Stats { return NewStatsWithClock(time.Now) }

// NewStatsWithClock injects a clock (tests).
func NewStatsWithClock(now func() time.Time) *Stats { return &Stats{now: now, started: now()} }

// RecordTokens registers n generated tokens at the current time.
func (s *Stats) RecordTokens(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.now()
	for i := 0; i < n; i++ {
		s.tokens.add(t)
	}
	s.totalTokens += int64(n)
}

// RecordRequest registers a completed request and its simulated payout.
func (s *Stats) RecordRequest(completionTokens int, payoutMicro int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests.add(s.now())
	s.totalRequests++
	s.earnedMicrocred += payoutMicro
	_ = completionTokens
}

// RequestStarted / RequestFinished track in-flight depth.
func (s *Stats) RequestStarted() {
	s.mu.Lock()
	s.inflight++
	s.mu.Unlock()
}

func (s *Stats) RequestFinished() {
	s.mu.Lock()
	if s.inflight > 0 {
		s.inflight--
	}
	s.mu.Unlock()
}

// Snapshot is a point-in-time view for the local API / heartbeat.
//
// Total* and EarnedMicrocred are LIFETIME figures: they continue across
// daemon restarts (since LifetimeSince). Session* are the same counters
// since this process started. The rates are rolling one-minute windows
// and know nothing of either.
type Snapshot struct {
	TokensPerSec1m  float64 `json:"tokens_per_sec_1m"`
	RequestsPerMin  float64 `json:"requests_per_min"`
	TotalRequests   int64   `json:"total_requests"`
	TotalTokens     int64   `json:"total_tokens"`
	Inflight        int     `json:"inflight"`
	EarnedMicrocred int64   `json:"earned_microcredits"`

	SessionRequests        int64     `json:"session_requests"`
	SessionTokens          int64     `json:"session_tokens"`
	SessionEarnedMicrocred int64     `json:"session_earned_microcredits"`
	LifetimeSince          time.Time `json:"lifetime_since"`
}

func (s *Stats) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	life := s.lifetimeLocked()
	return Snapshot{
		TokensPerSec1m:  float64(s.tokens.countSince(now.Add(-time.Minute))) / 60,
		RequestsPerMin:  float64(s.requests.countSince(now.Add(-time.Minute))),
		TotalRequests:   life.Requests,
		TotalTokens:     life.Tokens,
		Inflight:        s.inflight,
		EarnedMicrocred: life.EarnedMicrocred,

		SessionRequests:        s.totalRequests,
		SessionTokens:          s.totalTokens,
		SessionEarnedMicrocred: s.earnedMicrocred,
		LifetimeSince:          life.Since,
	}
}

// HeartbeatInput carries the non-stats fields the builder needs.
type HeartbeatInput struct {
	State      typesv1.NodeState
	QueueDepth int
	GPUTempC   float64
	OnBattery  bool
	VRAMUsedMB uint64
	RAMUsedMB  uint64
	Models     []*typesv1.ModelState
}

// BuildHeartbeat assembles the tunnel heartbeat message (SPEC §4.1
// telemetry: capacity, queue depth, temps, tok/s rolling stats).
func (s *Stats) BuildHeartbeat(in HeartbeatInput) *tunnelv1.Heartbeat {
	snap := s.Snapshot()
	return &tunnelv1.Heartbeat{
		State:           in.State,
		QueueDepth:      uint32(in.QueueDepth),
		TokensPerSec_1M: snap.TokensPerSec1m,
		GpuTempCelsius:  in.GPUTempC,
		OnBattery:       in.OnBattery,
		VramUsedMb:      in.VRAMUsedMB,
		RamUsedMb:       in.RAMUsedMB,
		Models:          in.Models,
		At:              timestamppb.New(s.now()),
	}
}

// ring is a compacting slice of event timestamps within the last ~5m.
type ring struct {
	ts []time.Time
}

func (r *ring) add(t time.Time) {
	r.ts = append(r.ts, t)
	if len(r.ts) > 65536 {
		r.compact(t.Add(-5 * time.Minute))
	}
}

func (r *ring) compact(cutoff time.Time) {
	keep := r.ts[:0]
	for _, t := range r.ts {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	r.ts = keep
}

func (r *ring) countSince(cutoff time.Time) int {
	n := 0
	for i := len(r.ts) - 1; i >= 0; i-- {
		if !r.ts[i].After(cutoff) {
			break
		}
		n++
	}
	return n
}
