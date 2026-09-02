package tunnel_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/teraflock/flockd/internal/governor"
	rt "github.com/teraflock/flockd/internal/runtime"
	"github.com/teraflock/flockd/internal/tunnel"
	"github.com/teraflock/flockd/internal/tunnel/fakecoord"
	tunnelv1 "github.com/teraflock/proto/gen/go/flock/tunnel/v1"
	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
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
