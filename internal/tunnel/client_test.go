package tunnel_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	flockengine "github.com/teraflock/flockd/internal/engine"
	"github.com/teraflock/flockd/internal/governor"
	rt "github.com/teraflock/flockd/internal/runtime"
	"github.com/teraflock/flockd/internal/tunnel"
	"github.com/teraflock/flockd/internal/tunnel/fakecoord"
	"github.com/teraflock/flockd/internal/update"
	tunnelv1 "github.com/teraflock/proto/gen/go/flock/tunnel/v1"
	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type engine struct{ inst rt.Instance }

func (e engine) Complete(ctx context.Context, req rt.CompletionRequest) (rt.TokenStream, error) {
	return e.inst.Complete(ctx, req)
}

type harness struct {
	coord  *fakecoord.Coordinator
	client *tunnel.Client
	cancel context.CancelFunc
}

func newHarness(t *testing.T, opts func(*tunnel.Options)) *harness {
	t.Helper()
	coord, err := fakecoord.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(coord.Stop)

	mock := rt.NewMockRuntime(0)
	inst, err := mock.Load(context.Background(), rt.ModelSpec{ID: "mock-8b"}, rt.ResourceBudget{MaxConcurrent: 4})
	if err != nil {
		t.Fatal(err)
	}

	coord.Allow("test-node") // stand in for enrollment; sessions must be known

	o := tunnel.Options{
		Dialer:            coord.Dialer(),
		Addr:              coord.Addr(),
		NodeID:            "test-node",
		CoordinatorPubKey: coord.PubKey(),
		Engine:            engine{inst},
		HeartbeatInterval: 50 * time.Millisecond,
		ReconnectMin:      20 * time.Millisecond,
		ReconnectMax:      100 * time.Millisecond,
		Log:               quietLog(),
		Hello: func() *tunnelv1.Hello {
			return &tunnelv1.Hello{NodeId: "test-node", DaemonVersion: "test"}
		},
		Heartbeat: func() *tunnelv1.Heartbeat {
			return &tunnelv1.Heartbeat{State: typesv1.NodeState_NODE_STATE_READY}
		},
	}
	if opts != nil {
		opts(&o)
	}
	client, err := tunnel.NewClient(o)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go client.Run(ctx)
	if !coord.WaitForSession(5 * time.Second) {
		t.Fatal("session never established")
	}
	return &harness{coord: coord, client: client, cancel: cancel}
}

func collect(t *testing.T, tokens <-chan *tunnelv1.TokenChunk, timeout time.Duration) (string, *tunnelv1.TokenChunk) {
	t.Helper()
	var sb strings.Builder
	deadline := time.After(timeout)
	for {
		select {
		case c, ok := <-tokens:
			if !ok {
				t.Fatal("token channel closed without done chunk")
			}
			sb.WriteString(c.GetDelta())
			if c.GetDone() {
				return sb.String(), c
			}
		case <-deadline:
			t.Fatal("timed out collecting tokens")
		}
	}
}

func TestDispatchStreamsTokens(t *testing.T) {
	h := newHarness(t, nil)
	_, acks, tokens, err := h.coord.Dispatch(fakecoord.DispatchOpts{
		ModelID: "mock-8b",
		Kind:    typesv1.RequestKind_REQUEST_KIND_CHAT,
		Messages: []*typesv1.ChatMessage{
			{Role: "user", Content: "hello mesh"},
		},
		Params: &typesv1.GenerationParams{Seed: 7, MaxTokens: 24},
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case ack := <-acks:
		if !ack.GetAccepted() {
			t.Fatalf("dispatch rejected: %s", ack.GetRejectReason())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no ack")
	}
	text, final := collect(t, tokens, 5*time.Second)
	if text == "" {
		t.Fatal("no tokens streamed")
	}
	if final.GetUsage().GetCompletionTokens() == 0 {
		t.Errorf("final chunk usage = %+v", final.GetUsage())
	}
	if final.GetFinishReason() == typesv1.FinishReason_FINISH_REASON_ERROR {
		t.Errorf("finish = error: %s", final.GetError())
	}
}

func TestDispatchRejectsBadSignature(t *testing.T) {
	h := newHarness(t, nil)
	_, acks, _, err := h.coord.Dispatch(fakecoord.DispatchOpts{
		ModelID:          "mock-8b",
		Kind:             typesv1.RequestKind_REQUEST_KIND_CHAT,
		Messages:         []*typesv1.ChatMessage{{Role: "user", Content: "evil"}},
		CorruptSignature: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case ack := <-acks:
		if ack.GetAccepted() {
			t.Fatal("dispatch with corrupted signature was accepted")
		}
		if ack.GetRejectReason() != "bad-signature" {
			t.Errorf("reject reason = %q", ack.GetRejectReason())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no rejection ack")
	}
}

func TestDispatchRejectedWhenYielded(t *testing.T) {
	gov := governor.New(governor.Policy{Serve: "idle-only", IdleAfter: time.Minute},
		&governor.FakeIdleSource{}, &governor.FakePowerSource{}, nil, quietLog())
	// idle-only + idle 0 => starts and stays Yielded.
	h := newHarness(t, func(o *tunnel.Options) { o.Admit = gov })
	_, acks, _, err := h.coord.Dispatch(fakecoord.DispatchOpts{
		ModelID:  "mock-8b",
		Kind:     typesv1.RequestKind_REQUEST_KIND_CHAT,
		Messages: []*typesv1.ChatMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ack := <-acks
	if ack.GetAccepted() || ack.GetRejectReason() != "yielded" {
		t.Fatalf("ack = %+v, want yielded rejection", ack)
	}
}

func TestCancelMidStream(t *testing.T) {
	coord, err := fakecoord.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(coord.Stop)
	coord.Allow("n")
	mock := rt.NewMockRuntime(20) // slow tokens so cancel lands mid-stream
	inst, _ := mock.Load(context.Background(), rt.ModelSpec{ID: "mock-8b"}, rt.ResourceBudget{MaxConcurrent: 4})
	client, err := tunnel.NewClient(tunnel.Options{
		Dialer: coord.Dialer(), Addr: coord.Addr(), NodeID: "n",
		CoordinatorPubKey: coord.PubKey(), Engine: engine{inst}, Log: quietLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.Run(ctx)
	if !coord.WaitForSession(5 * time.Second) {
		t.Fatal("no session")
	}

	id, acks, tokens, err := coord.Dispatch(fakecoord.DispatchOpts{
		ModelID:  "mock-8b",
		Kind:     typesv1.RequestKind_REQUEST_KIND_CHAT,
		Messages: []*typesv1.ChatMessage{{Role: "user", Content: "long"}},
		Params:   &typesv1.GenerationParams{Seed: 1, MaxTokens: 10000},
	})
	if err != nil {
		t.Fatal(err)
	}
	<-acks
	// Let a couple of tokens flow, then cancel.
	<-tokens
	if err := coord.Cancel(id, "operator-activity"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(10 * time.Second)
	for {
		select {
		case c, ok := <-tokens:
			if !ok {
				t.Fatal("closed without done")
			}
			if c.GetDone() {
				if c.GetFinishReason() != typesv1.FinishReason_FINISH_REASON_CANCELLED {
					t.Fatalf("finish = %v, want cancelled", c.GetFinishReason())
				}
				return
			}
		case <-deadline:
			t.Fatal("cancel never terminated the stream")
		}
	}
}

func TestChallengeDeterministicHash(t *testing.T) {
	h := newHarness(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r1, err := h.coord.Challenge(ctx, "mock-8b", "fingerprint probe")
	if err != nil {
		t.Fatal(err)
	}
	if r1.GetOutput() == "" || r1.GetOutputSha256() == "" {
		t.Fatalf("challenge response = %+v", r1)
	}
	r2, err := h.coord.Challenge(ctx, "mock-8b", "fingerprint probe")
	if err != nil {
		t.Fatal(err)
	}
	if r1.GetOutputSha256() != r2.GetOutputSha256() {
		t.Error("same seed challenge produced different hashes (fingerprinting broken)")
	}
}

func TestHeartbeatsFlow(t *testing.T) {
	h := newHarness(t, nil)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(h.coord.Heartbeats()) >= 2 {
			hb := h.coord.Heartbeats()[0]
			if hb.GetState() != typesv1.NodeState_NODE_STATE_READY {
				t.Fatalf("heartbeat state = %v", hb.GetState())
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no heartbeats received")
}

func TestDrainRejectsNewWork(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.coord.Drain("maintenance"); err != nil {
		t.Fatal(err)
	}
	// Drain is processed async; retry until the reject flips.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, acks, _, err := h.coord.Dispatch(fakecoord.DispatchOpts{
			ModelID:  "mock-8b",
			Kind:     typesv1.RequestKind_REQUEST_KIND_CHAT,
			Messages: []*typesv1.ChatMessage{{Role: "user", Content: "hi"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		ack := <-acks
		if !ack.GetAccepted() && ack.GetRejectReason() == "draining" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("drain never took effect")
}

func TestReconnectAfterKick(t *testing.T) {
	h := newHarness(t, nil)
	if h.client.Sessions() != 1 {
		t.Fatalf("sessions = %d", h.client.Sessions())
	}
	h.coord.KickSession()
	if !h.coord.WaitForSession(10 * time.Second) {
		t.Fatal("client never reconnected")
	}
	// The coordinator sees the new stream before the client has finished
	// its side of the handshake and bumped the counter; give it a moment.
	deadline := time.Now().Add(5 * time.Second)
	for h.client.Sessions() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if h.client.Sessions() < 2 {
		t.Fatalf("sessions = %d, want >= 2", h.client.Sessions())
	}
	// New session is fully functional.
	_, acks, tokens, err := h.coord.Dispatch(fakecoord.DispatchOpts{
		ModelID:  "mock-8b",
		Kind:     typesv1.RequestKind_REQUEST_KIND_CHAT,
		Messages: []*typesv1.ChatMessage{{Role: "user", Content: "post-reconnect"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	<-acks
	text, _ := collect(t, tokens, 5*time.Second)
	if text == "" {
		t.Fatal("no tokens after reconnect")
	}
}

func TestEmbeddingDispatch(t *testing.T) {
	h := newHarness(t, nil)
	ch, err := h.coord.DispatchEmbedding("mock-8b", []string{"alpha", "beta"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case res := <-ch:
		if res.GetDims() != 64 || len(res.GetEmbeddings()) != 128 {
			t.Fatalf("dims=%d len=%d", res.GetDims(), len(res.GetEmbeddings()))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no embedding result")
	}
}

func TestSignatureRoundTrip(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	d := &tunnelv1.DispatchRequest{
		RequestId: "r1",
		ModelId:   "m",
		Messages:  []*typesv1.ChatMessage{{Role: "user", Content: "hi"}},
		Params:    &typesv1.GenerationParams{Seed: 42},
	}
	if err := tunnel.SignDispatch(priv, d); err != nil {
		t.Fatal(err)
	}
	if err := tunnel.VerifyDispatch(pub, d); err != nil {
		t.Fatal(err)
	}
	// Any field mutation breaks the signature.
	d.Prompt = "tampered"
	if err := tunnel.VerifyDispatch(pub, d); err == nil {
		t.Fatal("tampered dispatch verified")
	}
}

// TestSessionRejectsUnenrolledNodeID pins the contract that broke the first
// live flockd↔coordinator mesh: a session must announce the node ID the
// coordinator assigned at enrollment, not the node's key fingerprint. The
// real coordinator rejects anything else with "unknown node", so the fake
// does too — otherwise this only surfaces against a deployed control plane.
func TestSessionRejectsUnenrolledNodeID(t *testing.T) {
	coord, err := fakecoord.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(coord.Stop)
	// Deliberately do NOT call coord.Allow — this node never enrolled.

	mock := rt.NewMockRuntime(0)
	inst, err := mock.Load(context.Background(), rt.ModelSpec{ID: "mock-8b"}, rt.ResourceBudget{MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	client, err := tunnel.NewClient(tunnel.Options{
		Dialer: coord.Dialer(), Addr: coord.Addr(), NodeID: "never-enrolled",
		CoordinatorPubKey: coord.PubKey(), Engine: engine{inst},
		ReconnectMin: 10 * time.Millisecond, ReconnectMax: 20 * time.Millisecond,
		Log: quietLog(),
		Hello: func() *tunnelv1.Hello {
			return &tunnelv1.Hello{NodeId: "never-enrolled", DaemonVersion: "test"}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.Run(ctx)

	if coord.WaitForSession(500 * time.Millisecond) {
		t.Fatal("session established for a node that never enrolled")
	}
}

// A ConfigUpdate applies mid-session: the heartbeat cadence changes on
// the running loop and the concurrency cap becomes a ceiling.
func TestConfigUpdateAppliesLive(t *testing.T) {
	h := newHarness(t, func(o *tunnel.Options) {
		o.HeartbeatInterval = time.Hour // nothing until the update lands
		o.MaxConcurrent = 8
	})
	if err := h.coord.PushConfig(&tunnelv1.ConfigUpdate{HeartbeatIntervalSeconds: 1, MaxConcurrentRequests: 1}); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for len(h.coord.Heartbeats()) < 2 {
		select {
		case <-deadline:
			t.Fatalf("heartbeat cadence not re-armed: %d heartbeats", len(h.coord.Heartbeats()))
		case <-time.After(20 * time.Millisecond):
		}
	}
	if got := h.client.EffectiveMaxConcurrent(); got != 1 {
		t.Fatalf("effective cap = %d, want 1 (min of operator 8, coordinator 1)", got)
	}
	// The coordinator can only lower, never raise past the operator.
	if err := h.coord.PushConfig(&tunnelv1.ConfigUpdate{MaxConcurrentRequests: 50}); err != nil {
		t.Fatal(err)
	}
	deadline = time.After(2 * time.Second)
	for h.client.EffectiveMaxConcurrent() != 8 {
		select {
		case <-deadline:
			t.Fatalf("effective cap = %d, want 8", h.client.EffectiveMaxConcurrent())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Over the effective cap, dispatches are rejected fast (over-capacity)
// so the coordinator retries elsewhere instead of queueing here.
func TestOverCapacityRejects(t *testing.T) {
	coord, err := fakecoord.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(coord.Stop)
	coord.Allow("n")
	mock := rt.NewMockRuntime(20) // slow: the first dispatch stays in flight
	inst, _ := mock.Load(context.Background(), rt.ModelSpec{ID: "mock-8b"}, rt.ResourceBudget{MaxConcurrent: 4})
	client, err := tunnel.NewClient(tunnel.Options{
		Dialer: coord.Dialer(), Addr: coord.Addr(), NodeID: "n",
		CoordinatorPubKey: coord.PubKey(), Engine: engine{inst}, Log: quietLog(),
		MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.Run(ctx)
	if !coord.WaitForSession(5 * time.Second) {
		t.Fatal("no session")
	}
	opts := fakecoord.DispatchOpts{
		ModelID:  "mock-8b",
		Kind:     typesv1.RequestKind_REQUEST_KIND_CHAT,
		Messages: []*typesv1.ChatMessage{{Role: "user", Content: "long"}},
		Params:   &typesv1.GenerationParams{Seed: 1, MaxTokens: 10000},
	}
	id1, acks1, tokens1, err := coord.Dispatch(opts)
	if err != nil {
		t.Fatal(err)
	}
	if a := <-acks1; !a.GetAccepted() {
		t.Fatalf("first dispatch rejected: %s", a.GetRejectReason())
	}
	<-tokens1 // in flight
	_, acks2, _, err := coord.Dispatch(opts)
	if err != nil {
		t.Fatal(err)
	}
	if a := <-acks2; a.GetAccepted() || a.GetRejectReason() != "over-capacity" {
		t.Fatalf("second dispatch ack = %+v, want over-capacity reject", a)
	}
	_ = coord.Cancel(id1, "done")
}

func TestSendModelStateReachesCoordinator(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.client.SendModelState(&typesv1.ModelState{ModelId: "m", State: "downloading"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	for {
		for _, m := range h.coord.ModelStates() {
			if m.GetModelId() == "m" && m.GetState() == "downloading" {
				return
			}
		}
		select {
		case <-deadline:
			t.Fatalf("model state never arrived: %v", h.coord.ModelStates())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Reasoning deltas travel on TokenChunk.reasoning, separate from delta,
// and are counted like any other token.
func TestDispatchRelaysReasoning(t *testing.T) {
	h := newHarness(t, func(o *tunnel.Options) {
		mock := &rt.MockRuntime{ReasoningTokens: 4}
		inst, err := mock.Load(context.Background(), rt.ModelSpec{ID: "mock-8b"}, rt.ResourceBudget{MaxConcurrent: 4})
		if err != nil {
			t.Fatal(err)
		}
		o.Engine = engine{inst}
	})
	_, acks, tokens, err := h.coord.Dispatch(fakecoord.DispatchOpts{
		ModelID:  "mock-8b",
		Kind:     typesv1.RequestKind_REQUEST_KIND_CHAT,
		Messages: []*typesv1.ChatMessage{{Role: "user", Content: "hi"}},
		Params:   &typesv1.GenerationParams{Seed: 5, MaxTokens: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ack := <-acks; !ack.GetAccepted() {
		t.Fatalf("rejected: %s", ack.GetRejectReason())
	}
	var reasoning, text string
	var counted uint32
	deadline := time.After(5 * time.Second)
	for done := false; !done; {
		select {
		case c := <-tokens:
			reasoning += c.GetReasoning()
			text += c.GetDelta()
			counted += c.GetTokenCount()
			if c.GetDone() {
				if got := c.GetUsage().GetCompletionTokens(); got != counted {
					t.Errorf("usage completion_tokens = %d, relayed token_count sum = %d", got, counted)
				}
				done = true
			}
		case <-deadline:
			t.Fatal("timed out")
		}
	}
	if reasoning == "" || text == "" {
		t.Fatalf("reasoning=%q text=%q: both must be relayed", reasoning, text)
	}
}

// The coordinator's release channel in a ConfigUpdate reaches the update
// checker immediately: below_minimum is true as soon as the mesh says so.
func TestConfigUpdateReleaseChannel(t *testing.T) {
	upd := &update.Checker{Current: "0.4.2", Log: quietLog()}
	h := newHarness(t, func(o *tunnel.Options) { o.Releases = upd })
	if _, ok := upd.Last(); ok {
		t.Fatal("result before any update")
	}
	// Cadence-only updates carry no versions and leave the checker alone.
	if err := h.coord.PushConfig(&tunnelv1.ConfigUpdate{HeartbeatIntervalSeconds: 1}); err != nil {
		t.Fatal(err)
	}
	if err := h.coord.PushConfig(&tunnelv1.ConfigUpdate{
		LatestVersion: "0.5.1", MinimumVersion: "0.5.0", ReleaseUrl: "https://github.com/teraflock/flockd/releases/tag/v0.5.1",
	}); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for {
		if last, ok := upd.Last(); ok {
			if !last.BelowMinimum || !last.Available || last.Latest != "0.5.1" || last.Minimum != "0.5.0" ||
				last.Source != update.SourceMesh || !strings.HasSuffix(last.URL, "v0.5.1") {
				t.Fatalf("result = %+v", last)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("release channel never reached the checker")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// An EarningsSnapshot pushed by the coordinator is cached with its receipt
// time (docs#24): the local API serves it while fresh, and the observer
// callback sees it too.
func TestEarningsSnapshotCached(t *testing.T) {
	seen := make(chan *tunnelv1.EarningsSnapshot, 1)
	h := newHarness(t, func(o *tunnel.Options) {
		o.OnEarnings = func(es *tunnelv1.EarningsSnapshot) { seen <- es }
	})
	if _, ok := h.client.Earnings(); ok {
		t.Fatal("snapshot before any push")
	}
	before := time.Now()
	want := &tunnelv1.EarningsSnapshot{
		AvailableCredits: 1200, EscrowCredits: 350, CreditsPerUsd: 1_000_000,
		EarnedTodayCredits: 70, Earned_7DCredits: 350, LifetimePayoutCredits: 1550,
		AsOf: timestamppb.Now(), PushIntervalSeconds: 300,
	}
	if err := h.coord.PushEarnings(want); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-seen:
		if !proto.Equal(got, want) {
			t.Fatalf("observed %+v, want %+v", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("snapshot never observed")
	}
	st, ok := h.client.Earnings()
	if !ok || !proto.Equal(st.Snapshot, want) {
		t.Fatalf("cached = %+v (%v)", st.Snapshot, ok)
	}
	if st.ReceivedAt.Before(before) || !st.Fresh(time.Now()) {
		t.Fatalf("receipt %s not fresh", st.ReceivedAt)
	}
	// Two push intervals on, the daemon stops trusting it.
	if st.Fresh(st.ReceivedAt.Add(11 * time.Minute)) {
		t.Fatal("snapshot still fresh after two intervals")
	}
}

// Forward compatibility, on the wire: a CoordinatorMessage carrying a
// oneof member this build does not know arrives as unknown fields
// (GetMsg() == nil) and is dropped — the session stays up and the next
// known message is handled. This is the assumption behind adding
// EarningsSnapshot without a flag day for 0.5.x / 0.6.0 daemons, which
// have exactly this switch minus the earnings arm.
func TestUnknownCoordinatorMessageIgnored(t *testing.T) {
	h := newHarness(t, nil)
	// Field 999 of CoordinatorMessage, length-delimited: what a future
	// oneof member looks like to a daemon built before it existed.
	raw := protowire.AppendTag(nil, 999, protowire.BytesType)
	raw = protowire.AppendBytes(raw, []byte("from-the-future"))
	unknown := &tunnelv1.CoordinatorMessage{}
	if err := proto.Unmarshal(raw, unknown); err != nil {
		t.Fatal(err)
	}
	if unknown.GetMsg() != nil {
		t.Fatalf("unknown field parsed as a known member: %T", unknown.GetMsg())
	}
	if err := h.coord.PushRaw(unknown); err != nil {
		t.Fatal(err)
	}
	// A known message right behind it is handled on the same session.
	if err := h.coord.PushEarnings(&tunnelv1.EarningsSnapshot{EscrowCredits: 7, PushIntervalSeconds: 300}); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for {
		if st, ok := h.client.Earnings(); ok {
			if st.Snapshot.GetEscrowCredits() != 7 {
				t.Fatalf("snapshot = %+v", st.Snapshot)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("session did not survive an unknown coordinator message")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if n := h.client.Sessions(); n != 1 {
		t.Fatalf("sessions = %d: the unknown message caused a reconnect", n)
	}
}

// decisionProto is a wire DecisionInput with option keys deliberately out
// of alphabetical order.
func decisionProto() *typesv1.DecisionInput {
	return &typesv1.DecisionInput{
		StateJson: `"payouts failing for 3 days"`,
		Questions: []*typesv1.DecisionQuestion{
			{Id: "team", Type: typesv1.DecisionQuestionType_DECISION_QUESTION_TYPE_CHOICE, InstructionsJson: `"Which team?"`,
				Options: []*typesv1.DecisionOption{{Key: "technical"}, {Key: "billing", DescriptionJson: `"Payments"`}, {Key: "account"}}},
			{Id: "urgency", Type: typesv1.DecisionQuestionType_DECISION_QUESTION_TYPE_SCORE, InstructionsJson: `"How urgent?"`,
				Options: []*typesv1.DecisionOption{{Key: "0", DescriptionJson: `"can wait"`}, {Key: "1", DescriptionJson: `"now"`}}},
			{Id: "escalate", Type: typesv1.DecisionQuestionType_DECISION_QUESTION_TYPE_NOUL, InstructionsJson: `"Escalate?"`},
		},
	}
}

// decisionEngine serves decisions from a mock decision model and records
// the runtime requests it was given; fail, when set, is returned instead.
type decisionEngine struct {
	inst rt.Instance
	fail error
	reqs chan rt.CompletionRequest
}

func (e *decisionEngine) Complete(ctx context.Context, req rt.CompletionRequest) (rt.TokenStream, error) {
	e.reqs <- req
	if e.fail != nil {
		return nil, e.fail
	}
	return e.inst.Complete(ctx, req)
}

func newDecisionHarness(t *testing.T, fail error) (*harness, *decisionEngine) {
	t.Helper()
	inst, err := rt.NewMockRuntime(0).Load(context.Background(), rt.ModelSpec{ID: "mock-decision", Decision: true}, rt.ResourceBudget{})
	if err != nil {
		t.Fatal(err)
	}
	eng := &decisionEngine{inst: inst, fail: fail, reqs: make(chan rt.CompletionRequest, 4)}
	return newHarness(t, func(o *tunnel.Options) { o.Engine = eng }), eng
}

func checkDecisionAnswers(t *testing.T, answers []*typesv1.DecisionAnswer) {
	t.Helper()
	if len(answers) != 3 {
		t.Fatalf("answers = %d, want 3", len(answers))
	}
	team, urgency, escalate := answers[0], answers[1], answers[2]
	if team.GetQuestionId() != "team" || team.GetType() != typesv1.DecisionQuestionType_DECISION_QUESTION_TYPE_CHOICE || team.GetChoice() == "" {
		t.Fatalf("answer 0 = %v", team)
	}
	p := team.GetProbabilities()
	if len(p) != 3 || p[0].GetKey() != "technical" || p[1].GetKey() != "billing" || p[2].GetKey() != "account" {
		t.Fatalf("choice probabilities not in option order: %v", p)
	}
	if sum := p[0].GetProbability() + p[1].GetProbability() + p[2].GetProbability(); sum < 0.999999 || sum > 1.000001 {
		t.Fatalf("choice probabilities sum to %v", sum)
	}
	if urgency.GetQuestionId() != "urgency" || urgency.GetType() != typesv1.DecisionQuestionType_DECISION_QUESTION_TYPE_SCORE ||
		len(urgency.GetProbabilities()) != 2 || urgency.GetProbabilities()[0].GetKey() != "0" || urgency.GetProbabilities()[1].GetKey() != "1" {
		t.Fatalf("answer 1 = %v", urgency)
	}
	if escalate.GetQuestionId() != "escalate" || escalate.GetType() != typesv1.DecisionQuestionType_DECISION_QUESTION_TYPE_NOUL ||
		len(escalate.GetProbabilities()) != 0 {
		t.Fatalf("answer 2 = %v", escalate)
	}
}

func TestDecisionDispatch(t *testing.T) {
	h, eng := newDecisionHarness(t, nil)
	ch, err := h.coord.DispatchDecision("mock-decision", decisionProto())
	if err != nil {
		t.Fatal(err)
	}
	var res *tunnelv1.DecisionResult
	select {
	case res = <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("no decision result")
	}
	if res.GetError() != "" || res.GetInvalidInput() {
		t.Fatalf("result error=%q invalid_input=%v", res.GetError(), res.GetInvalidInput())
	}
	if res.GetRequestId() == "" {
		t.Fatal("result has no request id")
	}
	checkDecisionAnswers(t, res.GetAnswers())
	if u := res.GetUsage(); u.GetPromptTokens() == 0 || u.GetCompletionTokens() != 0 {
		t.Fatalf("usage = %v, want prompt tokens only", u)
	}

	// What the runtime was given: kind, model, and the input in wire order.
	req := <-eng.reqs
	if req.Kind != rt.KindDecision || req.Model != "mock-decision" || req.Decision == nil || req.Origin != "mesh" {
		t.Fatalf("runtime request = %+v", req)
	}
	in := req.Decision
	if in.StateJSON != `"payouts failing for 3 days"` || len(in.Questions) != 3 {
		t.Fatalf("runtime input = %+v", in)
	}
	q := in.Questions[0]
	if q.ID != "team" || q.Type != rt.DecisionChoice || q.InstructionsJSON != `"Which team?"` || len(q.Options) != 3 ||
		q.Options[0] != (rt.DecisionOption{Key: "technical"}) ||
		q.Options[1] != (rt.DecisionOption{Key: "billing", DescriptionJSON: `"Payments"`}) ||
		q.Options[2] != (rt.DecisionOption{Key: "account"}) {
		t.Fatalf("choice question = %+v", q)
	}
	if in.Questions[1].Type != rt.DecisionScore || in.Questions[2].Type != rt.DecisionNoul {
		t.Fatalf("question types = %v %v", in.Questions[1].Type, in.Questions[2].Type)
	}
}

func TestDecisionDispatchInvalidInput(t *testing.T) {
	// The runtime rejected the input itself (llama-server 400): the result
	// says so, so the coordinator does not retry it on another node.
	h, _ := newDecisionHarness(t, &rt.InvalidInputError{Msg: "input (3028 tokens) is larger than the max context size (2048 tokens)"})
	ch, err := h.coord.DispatchDecision("mock-decision", decisionProto())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case res := <-ch:
		if !res.GetInvalidInput() || !strings.Contains(res.GetError(), "larger than the max context size") || len(res.GetAnswers()) != 0 {
			t.Fatalf("result = %v", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no decision result")
	}
}

func TestDecisionDispatchRuntimeError(t *testing.T) {
	// Anything else (501 not a decision model, a crash) is an ordinary
	// error: retryable elsewhere, invalid_input stays false. It must
	// still come back as a DecisionResult, not a TokenChunk.
	h, _ := newDecisionHarness(t, errors.New("llamacpp: /v1/systemone: status 501 Not Implemented"))
	ch, err := h.coord.DispatchDecision("mock-decision", decisionProto())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case res := <-ch:
		if res.GetInvalidInput() || !strings.Contains(res.GetError(), "501") {
			t.Fatalf("result = %v", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no decision result")
	}
}

func TestDecisionDispatchSignatureCoversInput(t *testing.T) {
	// The dispatch signature is over the whole message, the decision
	// payload included: changing one option after signing must fail.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	d := &tunnelv1.DispatchRequest{
		RequestId: "req-1", ModelId: "laya",
		Kind:     typesv1.RequestKind_REQUEST_KIND_DECISION,
		Decision: decisionProto(),
	}
	if err := tunnel.SignDispatch(priv, d); err != nil {
		t.Fatal(err)
	}
	if err := tunnel.VerifyDispatch(pub, d); err != nil {
		t.Fatalf("signed decision dispatch does not verify: %v", err)
	}
	d.Decision.Questions[0].Options[0].Key = "tampered"
	if err := tunnel.VerifyDispatch(pub, d); !errors.Is(err, tunnel.ErrBadSignature) {
		t.Fatalf("tampered decision input verified: %v", err)
	}
	d.Decision.Questions[0].Options[0].Key = "technical"
	d.Decision.Questions[0].Options[0], d.Decision.Questions[0].Options[1] = d.Decision.Questions[0].Options[1], d.Decision.Questions[0].Options[0]
	if err := tunnel.VerifyDispatch(pub, d); !errors.Is(err, tunnel.ErrBadSignature) {
		t.Fatalf("reordered options verified: %v", err)
	}
}

func TestDecisionChallenge(t *testing.T) {
	h, eng := newDecisionHarness(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r1, err := h.coord.ChallengeDecision(ctx, "mock-decision", decisionProto())
	if err != nil {
		t.Fatal(err)
	}
	if r1.GetOutput() != "" || r1.GetOutputSha256() != "" || r1.GetCompletionTokens() != 0 {
		t.Fatalf("decision challenge carries text fields: %v", r1)
	}
	checkDecisionAnswers(t, r1.GetAnswers())
	if req := <-eng.reqs; req.Kind != rt.KindDecision || req.Model != "mock-decision" || req.Origin != "challenge" {
		t.Fatalf("challenge ran as %+v, want a decision on the challenged model", req)
	}
	// Same probe, same probabilities: what the coordinator's tolerance
	// comparison relies on.
	r2, err := h.coord.ChallengeDecision(ctx, "mock-decision", decisionProto())
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(&tunnelv1.ChallengeResponse{Answers: r1.GetAnswers()}, &tunnelv1.ChallengeResponse{Answers: r2.GetAnswers()}) {
		t.Fatalf("decision challenge not deterministic:\n%v\n%v", r1.GetAnswers(), r2.GetAnswers())
	}
}

func TestDecisionChallengeFailureSendsEmptyResponse(t *testing.T) {
	h, _ := newDecisionHarness(t, errors.New("model not loaded"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := h.coord.ChallengeDecision(ctx, "mock-decision", decisionProto())
	if err != nil {
		t.Fatal(err)
	}
	if r.GetChallengeId() == "" || len(r.GetAnswers()) != 0 {
		t.Fatalf("response = %v", r)
	}
}

// realEngine is the production serving funnel (internal/engine) with a
// chat model (the default) and a decision model loaded, recording nothing:
// what the tunnel sees in the daemon.
func realEngine(t *testing.T) *flockengine.Engine {
	t.Helper()
	eng := flockengine.New(nil, nil, nil)
	mock := rt.NewMockRuntime(0)
	for _, spec := range []rt.ModelSpec{{ID: "chat-default"}, {ID: "chat-other"}, {ID: "laya", Decision: true}} {
		inst, err := mock.Load(context.Background(), spec, rt.ResourceBudget{MaxConcurrent: 4})
		if err != nil {
			t.Fatal(err)
		}
		eng.Register(spec, inst)
	}
	return eng
}

// recordingEngine wraps an engine and records the model each request ran on.
type recordingEngine struct {
	inner tunnel.Engine
	reqs  chan rt.CompletionRequest
}

func (e *recordingEngine) Complete(ctx context.Context, req rt.CompletionRequest) (rt.TokenStream, error) {
	e.reqs <- req
	return e.inner.Complete(ctx, req)
}

func TestChallengeRunsOnTheNamedModel(t *testing.T) {
	rec := &recordingEngine{inner: realEngine(t), reqs: make(chan rt.CompletionRequest, 4)}
	h := newHarness(t, func(o *tunnel.Options) { o.Engine = rec })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// A model that is loaded but is not the node's default.
	r, err := h.coord.Challenge(ctx, "chat-other", "fingerprint probe")
	if err != nil {
		t.Fatal(err)
	}
	if req := <-rec.reqs; req.Model != "chat-other" || req.Kind != rt.KindCompletion || req.Origin != "challenge" {
		t.Fatalf("challenge ran as %+v, want a completion on chat-other", req)
	}
	sum := sha256.Sum256([]byte(r.GetOutput()))
	if r.GetOutput() == "" || r.GetOutputSha256() != hex.EncodeToString(sum[:]) {
		t.Fatalf("response = %v", r)
	}

	// An empty model_id keeps the old behaviour: the default model.
	if _, err := h.coord.Challenge(ctx, "", "fingerprint probe"); err != nil {
		t.Fatal(err)
	}
	if req := <-rec.reqs; req.Model != "" {
		t.Fatalf("empty model_id ran on %q, want the node default", req.Model)
	}
}

// A text challenge the node cannot run must not be answered at all: the
// coordinator hashes whatever output comes back, and the hash of an empty
// output is a wrong fingerprint — a ban. Silence is a timeout, which costs
// one failed dispatch.
func TestUnrunnableChallengeIsNotAnswered(t *testing.T) {
	for name, model := range map[string]string{
		"model not loaded":             "not-loaded",
		"text probe on decision model": "laya",
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, func(o *tunnel.Options) { o.Engine = realEngine(t) })
			ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
			defer cancel()
			r, err := h.coord.Challenge(ctx, model, "fingerprint probe")
			if err == nil {
				t.Fatalf("node answered a challenge it could not run: %v", r)
			}
			// The session is intact and a runnable challenge still works.
			ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel2()
			if r, err := h.coord.Challenge(ctx2, "chat-default", "fingerprint probe"); err != nil || r.GetOutput() == "" {
				t.Fatalf("follow-up challenge: %v %v", r, err)
			}
		})
	}
}

// The same when the governor refuses the work (operator is at the machine).
func TestChallengeWhileYieldedIsNotAnswered(t *testing.T) {
	idle := &governor.FakeIdleSource{} // idle 0 => active => yielded
	gov := governor.New(governor.Policy{Serve: "idle-only", IdleAfter: time.Minute}, idle, &governor.FakePowerSource{}, nil, quietLog())
	eng := flockengine.New(gov, nil, nil)
	inst, err := rt.NewMockRuntime(0).Load(context.Background(), rt.ModelSpec{ID: "chat-default"}, rt.ResourceBudget{})
	if err != nil {
		t.Fatal(err)
	}
	eng.Register(rt.ModelSpec{ID: "chat-default"}, inst)
	h := newHarness(t, func(o *tunnel.Options) { o.Engine = eng })
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if r, err := h.coord.Challenge(ctx, "chat-default", "fingerprint probe"); err == nil {
		t.Fatalf("yielded node answered a challenge: %v", r)
	}
}

// Chat, completion and embedding dispatches aimed at a decision model fail
// with the gateway's wording instead of reaching the runtime.
func TestGenerationDispatchToDecisionModelFails(t *testing.T) {
	const want = `model "laya" is a decision model: use POST /v1/systemone`
	h := newHarness(t, func(o *tunnel.Options) { o.Engine = realEngine(t) })

	for name, opts := range map[string]fakecoord.DispatchOpts{
		"chat":       {ModelID: "laya", Kind: typesv1.RequestKind_REQUEST_KIND_CHAT, Messages: []*typesv1.ChatMessage{{Role: "user", Content: "hi"}}},
		"completion": {ModelID: "laya", Kind: typesv1.RequestKind_REQUEST_KIND_COMPLETION, Prompt: "hi"},
	} {
		_, acks, tokens, err := h.coord.Dispatch(opts)
		if err != nil {
			t.Fatal(err)
		}
		if ack := <-acks; !ack.GetAccepted() {
			t.Fatalf("%s: dispatch not accepted: %v", name, ack)
		}
		text, last := collect(t, tokens, 5*time.Second)
		if text != "" || last == nil || !last.GetDone() ||
			last.GetFinishReason() != typesv1.FinishReason_FINISH_REASON_ERROR || last.GetError() != want {
			t.Fatalf("%s: text=%q last=%v", name, text, last)
		}
	}

	// An embedding dispatch fails as an EmbeddingResult: the coordinator
	// waits for nothing else.
	ch, err := h.coord.DispatchEmbedding("laya", []string{"alpha"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case res := <-ch:
		if res.GetError() != want || len(res.GetEmbeddings()) != 0 {
			t.Fatalf("embedding result = %v", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no embedding result for a failed embedding dispatch")
	}

	// The chat model next to it still serves, and the decision model still
	// answers decisions.
	_, _, tokens, err := h.coord.Dispatch(fakecoord.DispatchOpts{ModelID: "chat-default", Kind: typesv1.RequestKind_REQUEST_KIND_CHAT,
		Messages: []*typesv1.ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if text, last := collect(t, tokens, 5*time.Second); text == "" || last.GetError() != "" {
		t.Fatalf("chat model: text=%q last=%v", text, last)
	}
	dch, err := h.coord.DispatchDecision("laya", decisionProto())
	if err != nil {
		t.Fatal(err)
	}
	if res := <-dch; res.GetError() != "" || len(res.GetAnswers()) != 3 {
		t.Fatalf("decision result = %v", res)
	}
}
