// Package tunnel implements the node side of flock.tunnel.v1.TunnelService:
// the persistent, node-initiated session over which the coordinator pushes
// dispatches, challenges and model assignments (SPEC §3: nodes dial out,
// never listen).
package tunnel

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	rt "github.com/teraflock/flockd/internal/runtime"
	tunnelv1 "github.com/teraflock/proto/gen/go/flock/tunnel/v1"
	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
	"google.golang.org/grpc"
)

// Engine runs inference for dispatches and challenges. Satisfied by the
// daemon's engine wrapper around runtime.Instance.
type Engine interface {
	Complete(ctx context.Context, req rt.CompletionRequest) (rt.TokenStream, error)
}

// Admitter gates work through the governor. *governor.Governor satisfies it.
type Admitter interface {
	Admit(ctx context.Context, id string) (context.Context, func(), error)
}

// alwaysAdmit is used when no governor is wired (tests).
type alwaysAdmit struct{}

func (alwaysAdmit) Admit(ctx context.Context, _ string) (context.Context, func(), error) {
	c, cancel := context.WithCancel(ctx)
	return c, cancel, nil
}

// Options configures the Client.
type Options struct {
	Dialer Dialer
	Addr   string
	NodeID string
	// CoordinatorPubKey is the Ed25519 dispatch-signing key pinned at
	// enrollment. Required: dispatches with bad signatures are dropped.
	CoordinatorPubKey ed25519.PublicKey

	Engine Engine
	Admit  Admitter
	// Heartbeat returns a fresh heartbeat message (telemetry assembly).
	Heartbeat func() *tunnelv1.Heartbeat
	// Hello returns the session-open message (capability, models, budget).
	Hello func() *tunnelv1.Hello
	// OnModelAssignment is invoked for coordinator model placement pushes.
	OnModelAssignment func(ctx context.Context, ma *tunnelv1.ModelAssignment)

	HeartbeatInterval time.Duration
	ReconnectMin      time.Duration
	ReconnectMax      time.Duration
	Log               *slog.Logger
}

// Client maintains the session and handles pushed work.
type Client struct {
	o Options

	mu       sync.Mutex
	inflight map[string]context.CancelFunc
	draining bool
	sessions int // completed handshakes, for tests/telemetry
}

// NewClient validates options and builds a client.
func NewClient(o Options) (*Client, error) {
	if o.Dialer == nil || o.Addr == "" {
		return nil, errors.New("tunnel: dialer and addr are required")
	}
	if o.Engine == nil {
		return nil, errors.New("tunnel: engine is required")
	}
	if len(o.CoordinatorPubKey) != ed25519.PublicKeySize {
		return nil, errors.New("tunnel: pinned coordinator pubkey is required")
	}
	if o.Admit == nil {
		o.Admit = alwaysAdmit{}
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.HeartbeatInterval <= 0 {
		o.HeartbeatInterval = 5 * time.Second
	}
	if o.ReconnectMin <= 0 {
		o.ReconnectMin = time.Second
	}
	if o.ReconnectMax <= 0 {
		o.ReconnectMax = 2 * time.Minute
	}
	return &Client{o: o, inflight: map[string]context.CancelFunc{}}, nil
}

// Sessions returns the number of successful handshakes (reconnect tests).
func (c *Client) Sessions() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessions
}

// Run keeps the session alive until ctx is cancelled, reconnecting with
// jittered exponential backoff.
func (c *Client) Run(ctx context.Context) {
	backoff := c.o.ReconnectMin
	for {
		start := time.Now()
		err := c.runSession(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > 30*time.Second {
			backoff = c.o.ReconnectMin // session was healthy: reset
		}
		sleep := jitter(backoff)
		c.o.Log.Warn("tunnel session ended; reconnecting", "err", err, "backoff", sleep)
		select {
		case <-ctx.Done():
			return
		case <-time.After(sleep):
		}
		backoff = min(backoff*2, c.o.ReconnectMax)
	}
}

// jitter returns d +/- 30%.
func jitter(d time.Duration) time.Duration {
	f := 0.7 + 0.6*rand.Float64() //nolint:gosec // backoff jitter, not crypto
	return time.Duration(float64(d) * f)
}

// sessionStream is the mutex-guarded send side of the bidi stream.
type sessionStream struct {
	mu sync.Mutex
	s  grpc.BidiStreamingClient[tunnelv1.NodeMessage, tunnelv1.CoordinatorMessage]
}

func (ss *sessionStream) send(m *tunnelv1.NodeMessage) error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.s.Send(m)
}

func (c *Client) runSession(ctx context.Context) error {
	conn, err := c.o.Dialer.Dial(ctx, c.o.Addr)
	if err != nil {
		return err
	}
	defer conn.Close()

	sctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := tunnelv1.NewTunnelServiceClient(conn).Session(sctx)
	if err != nil {
		return fmt.Errorf("tunnel: open session: %w", err)
	}
	ss := &sessionStream{s: stream}

	hello := &tunnelv1.Hello{NodeId: c.o.NodeID}
	if c.o.Hello != nil {
		hello = c.o.Hello()
	}
	if err := ss.send(&tunnelv1.NodeMessage{Msg: &tunnelv1.NodeMessage_Hello{Hello: hello}}); err != nil {
		return fmt.Errorf("tunnel: send hello: %w", err)
	}

	first, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("tunnel: await hello ack: %w", err)
	}
	ack := first.GetHelloAck()
	if ack == nil {
		return fmt.Errorf("tunnel: first coordinator message was not HelloAck")
	}
	c.mu.Lock()
	c.sessions++
	c.draining = false
	c.mu.Unlock()
	c.o.Log.Info("tunnel session established", "session_id", ack.GetSessionId())

	hbInterval := c.o.HeartbeatInterval
	if s := ack.GetHeartbeatIntervalSeconds(); s > 0 {
		hbInterval = time.Duration(s) * time.Second
	}
	go c.heartbeatLoop(sctx, ss, hbInterval)

	// Graceful Goodbye on shutdown (SPEC A5.1).
	go func() {
		<-ctx.Done()
		c.mu.Lock()
		ids := make([]string, 0, len(c.inflight))
		for id := range c.inflight {
			ids = append(ids, id)
		}
		c.mu.Unlock()
		_ = ss.send(&tunnelv1.NodeMessage{Msg: &tunnelv1.NodeMessage_Goodbye{
			Goodbye: &tunnelv1.Goodbye{Reason: "shutdown", InflightRequestIds: ids},
		}})
		_ = stream.CloseSend()
	}()

	for {
		msg, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("tunnel: recv: %w", err)
		}
		c.handle(sctx, ss, msg)
	}
}

func (c *Client) heartbeatLoop(ctx context.Context, ss *sessionStream, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			hb := &tunnelv1.Heartbeat{}
			if c.o.Heartbeat != nil {
				hb = c.o.Heartbeat()
			}
			if err := ss.send(&tunnelv1.NodeMessage{Msg: &tunnelv1.NodeMessage_Heartbeat{Heartbeat: hb}}); err != nil {
				return
			}
		}
	}
}

func (c *Client) handle(ctx context.Context, ss *sessionStream, msg *tunnelv1.CoordinatorMessage) {
	switch m := msg.GetMsg().(type) {
	case *tunnelv1.CoordinatorMessage_Dispatch:
		c.handleDispatch(ctx, ss, m.Dispatch)
	case *tunnelv1.CoordinatorMessage_Cancel:
		c.handleCancel(m.Cancel)
	case *tunnelv1.CoordinatorMessage_Challenge:
		go c.handleChallenge(ctx, ss, m.Challenge)
	case *tunnelv1.CoordinatorMessage_ModelAssignment:
		if c.o.OnModelAssignment != nil {
			go c.o.OnModelAssignment(ctx, m.ModelAssignment)
		}
	case *tunnelv1.CoordinatorMessage_Drain:
		c.o.Log.Info("coordinator requested drain", "reason", m.Drain.GetReason())
		c.mu.Lock()
		c.draining = true
		c.mu.Unlock()
	case *tunnelv1.CoordinatorMessage_Config:
		// ConfigUpdate: heartbeat cadence changes take effect next session;
		// concurrency is enforced by the governor/budget. Logged for now.
		c.o.Log.Info("config update received", "heartbeat_s", m.Config.GetHeartbeatIntervalSeconds())
	case *tunnelv1.CoordinatorMessage_HelloAck:
		// Duplicate ack: ignore.
	}
}

func (c *Client) reject(ss *sessionStream, id, reason string) {
	_ = ss.send(&tunnelv1.NodeMessage{Msg: &tunnelv1.NodeMessage_DispatchAck{
		DispatchAck: &tunnelv1.DispatchAck{RequestId: id, Accepted: false, RejectReason: reason},
	}})
}

func (c *Client) handleDispatch(ctx context.Context, ss *sessionStream, d *tunnelv1.DispatchRequest) {
	id := d.GetRequestId()
	// Verify the coordinator signature before anything else (SPEC §6).
	if err := VerifyDispatch(c.o.CoordinatorPubKey, d); err != nil {
		c.o.Log.Error("rejecting dispatch with invalid signature", "request_id", id)
		c.reject(ss, id, "bad-signature")
		return
	}
	c.mu.Lock()
	draining := c.draining
	c.mu.Unlock()
	if draining {
		c.reject(ss, id, "draining")
		return
	}
	reqCtx, release, err := c.o.Admit.Admit(ctx, id)
	if err != nil {
		c.reject(ss, id, "yielded")
		return
	}
	if dl := d.GetDeadline(); dl != nil {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithDeadline(reqCtx, dl.AsTime())
		prev := release
		release = func() { cancel(); prev() }
	}

	c.mu.Lock()
	rc, rcCancel := context.WithCancel(reqCtx)
	c.inflight[id] = rcCancel
	c.mu.Unlock()

	_ = ss.send(&tunnelv1.NodeMessage{Msg: &tunnelv1.NodeMessage_DispatchAck{
		DispatchAck: &tunnelv1.DispatchAck{RequestId: id, Accepted: true},
	}})

	go func() {
		defer func() {
			c.mu.Lock()
			delete(c.inflight, id)
			c.mu.Unlock()
			rcCancel()
			release()
		}()
		c.runDispatch(rc, ss, d)
	}()
}

func (c *Client) runDispatch(ctx context.Context, ss *sessionStream, d *tunnelv1.DispatchRequest) {
	id := d.GetRequestId()
	req := toRuntimeRequest(d)

	stream, err := c.o.Engine.Complete(ctx, req)
	if err != nil {
		c.sendError(ss, d, err)
		return
	}
	defer stream.Close()

	if req.Kind == rt.KindEmbedding {
		c.relayEmbedding(ss, id, stream)
		return
	}

	for {
		chunk, rerr := stream.Recv()
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return
			}
			c.sendError(ss, d, rerr)
			return
		}
		tc := &tunnelv1.TokenChunk{
			RequestId:    id,
			Delta:        chunk.Delta,
			TokenCount:   uint32(chunk.TokenCount),
			Done:         chunk.Done,
			FinishReason: finishToProto(chunk.FinishReason),
			Error:        chunk.Err,
		}
		if chunk.Usage != nil {
			tc.Usage = &typesv1.Usage{
				PromptTokens:     uint32(chunk.Usage.PromptTokens),
				CompletionTokens: uint32(chunk.Usage.CompletionTokens),
			}
		}
		if err := ss.send(&tunnelv1.NodeMessage{Msg: &tunnelv1.NodeMessage_TokenChunk{TokenChunk: tc}}); err != nil {
			return // session died; reconnect loop takes over
		}
	}
}

func (c *Client) relayEmbedding(ss *sessionStream, id string, stream rt.TokenStream) {
	res := &tunnelv1.EmbeddingResult{RequestId: id}
	for {
		chunk, err := stream.Recv()
		if err != nil {
			break
		}
		if chunk.Err != "" {
			res.Error = chunk.Err
		}
		for _, vec := range chunk.Embeddings {
			res.Dims = uint32(len(vec))
			res.Embeddings = append(res.Embeddings, vec...)
		}
		if chunk.Usage != nil {
			res.Usage = &typesv1.Usage{PromptTokens: uint32(chunk.Usage.PromptTokens)}
		}
	}
	_ = ss.send(&tunnelv1.NodeMessage{Msg: &tunnelv1.NodeMessage_EmbeddingResult{EmbeddingResult: res}})
}

func (c *Client) sendError(ss *sessionStream, d *tunnelv1.DispatchRequest, err error) {
	finish := tunnelv1.TokenChunk{
		RequestId:    d.GetRequestId(),
		Done:         true,
		FinishReason: typesv1.FinishReason_FINISH_REASON_ERROR,
		Error:        err.Error(),
	}
	if errors.Is(err, context.Canceled) {
		finish.FinishReason = typesv1.FinishReason_FINISH_REASON_CANCELLED
		finish.Error = ""
	}
	_ = ss.send(&tunnelv1.NodeMessage{Msg: &tunnelv1.NodeMessage_TokenChunk{TokenChunk: &finish}})
}

func (c *Client) handleCancel(cr *tunnelv1.CancelRequest) {
	c.mu.Lock()
	cancel, ok := c.inflight[cr.GetRequestId()]
	c.mu.Unlock()
	if ok {
		c.o.Log.Info("cancelling dispatch", "request_id", cr.GetRequestId(), "reason", cr.GetReason())
		cancel()
	}
}

// handleChallenge runs a fingerprint probe: fixed-seed greedy decode whose
// output hash the coordinator compares against the private expected set
// (SPEC §2.2).
func (c *Client) handleChallenge(ctx context.Context, ss *sessionStream, ch *tunnelv1.Challenge) {
	req := rt.CompletionRequest{
		ID:     "challenge-" + ch.GetChallengeId(),
		Kind:   rt.KindCompletion,
		Prompt: ch.GetPrompt(),
		Params: paramsFromProto(ch.GetParams()),
	}
	start := time.Now()
	stream, err := c.o.Engine.Complete(ctx, req)
	resp := &tunnelv1.ChallengeResponse{ChallengeId: ch.GetChallengeId()}
	if err == nil {
		text, usage, _, derr := rt.Drain(stream)
		if derr == nil {
			sum := sha256.Sum256([]byte(text))
			resp.Output = text
			resp.OutputSha256 = hex.EncodeToString(sum[:])
			resp.CompletionTokens = uint32(usage.CompletionTokens)
			if el := time.Since(start).Seconds(); el > 0 {
				resp.TokensPerSec = float64(usage.CompletionTokens) / el
			}
		}
	}
	_ = ss.send(&tunnelv1.NodeMessage{Msg: &tunnelv1.NodeMessage_ChallengeResponse{ChallengeResponse: resp}})
}

func toRuntimeRequest(d *tunnelv1.DispatchRequest) rt.CompletionRequest {
	req := rt.CompletionRequest{
		ID:     d.GetRequestId(),
		Model:  d.GetModelId(),
		Params: paramsFromProto(d.GetParams()),
	}
	switch d.GetKind() {
	case typesv1.RequestKind_REQUEST_KIND_COMPLETION:
		req.Kind = rt.KindCompletion
		req.Prompt = d.GetPrompt()
	case typesv1.RequestKind_REQUEST_KIND_EMBEDDING:
		req.Kind = rt.KindEmbedding
		req.EmbeddingInput = d.GetEmbeddingInput()
	default:
		req.Kind = rt.KindChat
		for _, m := range d.GetMessages() {
			req.Messages = append(req.Messages, rt.Message{Role: m.GetRole(), Content: m.GetContent()})
		}
	}
	return req
}

func paramsFromProto(p *typesv1.GenerationParams) rt.GenerationParams {
	return rt.GenerationParams{
		Seed:             p.GetSeed(),
		Temperature:      p.GetTemperature(),
		TopP:             p.GetTopP(),
		MaxTokens:        int(p.GetMaxTokens()),
		Stop:             p.GetStop(),
		FrequencyPenalty: p.GetFrequencyPenalty(),
		PresencePenalty:  p.GetPresencePenalty(),
	}
}

func finishToProto(s string) typesv1.FinishReason {
	switch s {
	case "stop":
		return typesv1.FinishReason_FINISH_REASON_STOP
	case "length":
		return typesv1.FinishReason_FINISH_REASON_LENGTH
	case "cancelled":
		return typesv1.FinishReason_FINISH_REASON_CANCELLED
	case "error":
		return typesv1.FinishReason_FINISH_REASON_ERROR
	case "":
		return typesv1.FinishReason_FINISH_REASON_UNSPECIFIED
	default:
		return typesv1.FinishReason_FINISH_REASON_STOP
	}
}
