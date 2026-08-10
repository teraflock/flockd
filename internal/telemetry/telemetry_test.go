package telemetry

import (
	"testing"
	"time"

	typesv1 "github.com/hivegrid/proto/gen/go/hive/types/v1"
)

func TestRollingTokensPerSec(t *testing.T) {
	now := time.Unix(1000000, 0)
	s := NewStatsWithClock(func() time.Time { return now })

	s.RecordTokens(120) // 120 tokens right now
	snap := s.Snapshot()
	if snap.TokensPerSec1m != 2.0 { // 120 tokens / 60s window
		t.Errorf("tok/s = %v, want 2.0", snap.TokensPerSec1m)
	}

	// Advance past the window: rate decays to zero.
	now = now.Add(2 * time.Minute)
	if got := s.Snapshot().TokensPerSec1m; got != 0 {
		t.Errorf("tok/s after window = %v, want 0", got)
	}
	if s.Snapshot().TotalTokens != 120 {
		t.Errorf("total tokens = %d", s.Snapshot().TotalTokens)
	}
}

func TestInflightAndRequests(t *testing.T) {
	s := NewStats()
	s.RequestStarted()
	s.RequestStarted()
	s.RequestFinished()
	snap := s.Snapshot()
	if snap.Inflight != 1 {
		t.Errorf("inflight = %d", snap.Inflight)
	}
	s.RecordRequest(10, 55)
	if got := s.Snapshot(); got.TotalRequests != 1 || got.EarnedMicrocred != 55 {
		t.Errorf("snapshot = %+v", got)
	}
}

func TestBuildHeartbeat(t *testing.T) {
	now := time.Unix(2000000, 0)
	s := NewStatsWithClock(func() time.Time { return now })
	s.RecordTokens(60)
	hb := s.BuildHeartbeat(HeartbeatInput{
		State:      typesv1.NodeState_NODE_STATE_READY,
		QueueDepth: 3,
		OnBattery:  true,
	})
	if hb.State != typesv1.NodeState_NODE_STATE_READY || hb.QueueDepth != 3 || !hb.OnBattery {
		t.Errorf("heartbeat = %+v", hb)
	}
	if hb.TokensPerSec_1M != 1.0 {
		t.Errorf("tok/s = %v", hb.TokensPerSec_1M)
	}
	if !hb.At.AsTime().Equal(now) {
		t.Errorf("at = %v", hb.At.AsTime())
	}
}
