// Package fakecoord is an in-process fake coordinator implementing
// flock.tunnel.v1.TunnelService over bufconn (SPEC §A2.2 key seam). It lets
// the whole daemon — enrollment, session, heartbeats, dispatch, challenges —
// run without a control plane. `flockd --standalone` uses it in Phase 0, and
// it doubles as the test harness for the tunnel client.
package fakecoord

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"sync"
	"time"

	"github.com/teraflock/flockd/internal/tunnel"
	tunnelv1 "github.com/teraflock/proto/gen/go/flock/tunnel/v1"
	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Coordinator is the fake. Zero external I/O; everything flows over bufconn.
type Coordinator struct {
	tunnelv1.UnimplementedTunnelServiceServer

	signPriv ed25519.PrivateKey
	signPub  ed25519.PublicKey

	caCert *x509.Certificate
	caKey  ed25519.PrivateKey

	lis *bufconn.Listener
	srv *grpc.Server

	mu         sync.Mutex
	session    *session
	heartbeats []*tunnelv1.Heartbeat
	hello      *tunnelv1.Hello
	enrolled   map[string]bool
	reqSeq     int
	pending    map[string]chan *tunnelv1.TokenChunk
	pendingEmb map[string]chan *tunnelv1.EmbeddingResult
	pendingCh  map[string]chan *tunnelv1.ChallengeResponse
	acks       map[string]chan *tunnelv1.DispatchAck
	sessionUp  chan struct{}
}

type session struct {
	stream grpc.BidiStreamingServer[tunnelv1.NodeMessage, tunnelv1.CoordinatorMessage]
	sendMu sync.Mutex
	kick   chan struct{}
}

func (s *session) send(m *tunnelv1.CoordinatorMessage) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.stream.Send(m)
}

// New starts the fake coordinator on an in-memory listener.
func New() (*Coordinator, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("fakecoord: generate signing key: %w", err)
	}
	caCert, caKey, err := newCA()
	if err != nil {
		return nil, err
	}
	c := &Coordinator{
		signPriv: priv,
		signPub:  pub,
		caCert:   caCert,
		caKey:    caKey,
		lis:      bufconn.Listen(1 << 20),
		// Must mirror the real coordinator's enforcement policy. gRPC's
		// default rejects clients pinging more often than every 5 minutes
		// with GOAWAY "too_many_pings" — and the daemon pings every 20s
		// (tunnel.PingInterval), so a default server here would drop every
		// standalone session on a timer.
		srv: grpc.NewServer(grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second,
			PermitWithoutStream: true,
		})),
		enrolled:   map[string]bool{},
		pending:    map[string]chan *tunnelv1.TokenChunk{},
		pendingEmb: map[string]chan *tunnelv1.EmbeddingResult{},
		pendingCh:  map[string]chan *tunnelv1.ChallengeResponse{},
		acks:       map[string]chan *tunnelv1.DispatchAck{},
		sessionUp:  make(chan struct{}),
	}
	tunnelv1.RegisterTunnelServiceServer(c.srv, c)
	go func() { _ = c.srv.Serve(c.lis) }()
	return c, nil
}

// Stop tears down the server.
func (c *Coordinator) Stop() { c.srv.Stop() }

// PubKey is the dispatch-signing key nodes pin at enrollment.
func (c *Coordinator) PubKey() ed25519.PublicKey { return c.signPub }

// Dialer returns a tunnel.Dialer that connects to this fake over bufconn.
func (c *Coordinator) Dialer() tunnel.Dialer { return bufDialer{lis: c.lis} }

// Addr is a placeholder address for logs/config plumbing.
func (c *Coordinator) Addr() string { return "fakecoord:bufconn" }

// Allow registers a node ID as enrolled without running the Enroll RPC, for
// tests that drive the session directly with a synthetic identity.
func (c *Coordinator) Allow(nodeID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.enrolled[nodeID] = true
}

type bufDialer struct{ lis *bufconn.Listener }

func (d bufDialer) Dial(_ context.Context, _ string) (*grpc.ClientConn, error) {
	return grpc.NewClient("passthrough:///fakecoord",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return d.lis.DialContext(ctx)
		}),
		grpc.WithInsecure(), //nolint:staticcheck // in-memory transport
	)
}

// ---- TunnelService ----

// Enroll signs the node's CSR with the fake CA. Claim code "let-me-in" (or
// any non-empty code) is accepted; real validation is a control-plane
// concern.
func (c *Coordinator) Enroll(_ context.Context, req *tunnelv1.EnrollRequest) (*tunnelv1.EnrollResponse, error) {
	if req.GetClaimCode() == "" {
		return nil, fmt.Errorf("fakecoord: empty claim code")
	}
	if len(req.GetPubkey()) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("fakecoord: bad node pubkey")
	}
	nodeID := fmt.Sprintf("node-%x", req.GetPubkey()[:6])
	certPEM, expires, err := c.signCSR(req.GetCsrPem(), nodeID)
	if err != nil {
		return nil, err
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.caCert.Raw})

	c.mu.Lock()
	c.enrolled[nodeID] = true
	c.mu.Unlock()

	return &tunnelv1.EnrollResponse{
		NodeId:            nodeID,
		ClientCertPem:     certPEM,
		CaCertPem:         caPEM,
		CoordinatorPubkey: c.signPub,
		CertExpiresAt:     timestamppb.New(expires),
	}, nil
}

// Session implements the persistent bidi tunnel.
func (c *Coordinator) Session(stream grpc.BidiStreamingServer[tunnelv1.NodeMessage, tunnelv1.CoordinatorMessage]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return fmt.Errorf("fakecoord: first node message must be Hello")
	}
	// Match the real coordinator: a session must announce the node ID that
	// enrollment assigned, not the node's raw key fingerprint. Accepting
	// anything here would hide node-identity bugs until they reach a live
	// mesh, which is exactly what this fake exists to prevent.
	c.mu.Lock()
	known := c.enrolled[hello.GetNodeId()]
	c.mu.Unlock()
	if !known {
		return fmt.Errorf("fakecoord: unknown node %q (enroll first)", hello.GetNodeId())
	}

	sess := &session{stream: stream, kick: make(chan struct{})}
	c.mu.Lock()
	c.session = sess
	c.hello = hello
	up := c.sessionUp
	c.sessionUp = make(chan struct{})
	c.mu.Unlock()
	close(up)

	if err := sess.send(&tunnelv1.CoordinatorMessage{Msg: &tunnelv1.CoordinatorMessage_HelloAck{
		HelloAck: &tunnelv1.HelloAck{
			SessionId:                fmt.Sprintf("sess-%d", time.Now().UnixNano()),
			HeartbeatIntervalSeconds: 0, // node default
			MinSupportedVersion:      "0.0.1",
		},
	}}); err != nil {
		return err
	}

	recvDone := make(chan struct{})
	go func() {
		defer close(recvDone)
		for {
			msg, err := stream.Recv()
			if err != nil {
				return
			}
			c.handleNodeMessage(msg)
		}
	}()

	defer func() {
		c.mu.Lock()
		if c.session == sess {
			c.session = nil
		}
		c.mu.Unlock()
	}()
	select {
	case <-recvDone:
		return nil
	case <-sess.kick:
		return fmt.Errorf("fakecoord: session kicked")
	}
}

func (c *Coordinator) handleNodeMessage(msg *tunnelv1.NodeMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch m := msg.GetMsg().(type) {
	case *tunnelv1.NodeMessage_Heartbeat:
		c.heartbeats = append(c.heartbeats, m.Heartbeat)
	case *tunnelv1.NodeMessage_TokenChunk:
		if ch, ok := c.pending[m.TokenChunk.GetRequestId()]; ok {
			ch <- m.TokenChunk
			if m.TokenChunk.GetDone() {
				close(ch)
				delete(c.pending, m.TokenChunk.GetRequestId())
			}
		}
	case *tunnelv1.NodeMessage_EmbeddingResult:
		if ch, ok := c.pendingEmb[m.EmbeddingResult.GetRequestId()]; ok {
			ch <- m.EmbeddingResult
			close(ch)
			delete(c.pendingEmb, m.EmbeddingResult.GetRequestId())
		}
	case *tunnelv1.NodeMessage_ChallengeResponse:
		if ch, ok := c.pendingCh[m.ChallengeResponse.GetChallengeId()]; ok {
			ch <- m.ChallengeResponse
			close(ch)
			delete(c.pendingCh, m.ChallengeResponse.GetChallengeId())
		}
	case *tunnelv1.NodeMessage_DispatchAck:
		if ch, ok := c.acks[m.DispatchAck.GetRequestId()]; ok {
			ch <- m.DispatchAck
			close(ch)
			delete(c.acks, m.DispatchAck.GetRequestId())
		}
	case *tunnelv1.NodeMessage_Goodbye:
		// Node draining/shutting down; nothing to route in the fake.
	case *tunnelv1.NodeMessage_ModelState:
	case *tunnelv1.NodeMessage_Hello:
	}
}

// ---- driver API (tests / standalone) ----

// WaitForSession blocks until a node session is established.
func (c *Coordinator) WaitForSession(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		up := c.session != nil
		c.mu.Unlock()
		if up {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// Heartbeats returns all heartbeats received so far.
func (c *Coordinator) Heartbeats() []*tunnelv1.Heartbeat {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*tunnelv1.Heartbeat(nil), c.heartbeats...)
}

// LastHello returns the most recent session Hello.
func (c *Coordinator) LastHello() *tunnelv1.Hello {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hello
}

func (c *Coordinator) sessionOrErr() (*session, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session == nil {
		return nil, fmt.Errorf("fakecoord: no active node session")
	}
	return c.session, nil
}

// DispatchOpts customizes a driven dispatch.
type DispatchOpts struct {
	ModelID  string
	Kind     typesv1.RequestKind
	Messages []*typesv1.ChatMessage
	Prompt   string
	Input    []string
	Params   *typesv1.GenerationParams
	Deadline time.Time
	// CorruptSignature sends an invalid signature (adversarial tests).
	CorruptSignature bool
}

// Dispatch pushes a signed DispatchRequest down the session and returns the
// ack plus a channel of TokenChunks (closed after the final chunk).
func (c *Coordinator) Dispatch(opts DispatchOpts) (string, <-chan *tunnelv1.DispatchAck, <-chan *tunnelv1.TokenChunk, error) {
	sess, err := c.sessionOrErr()
	if err != nil {
		return "", nil, nil, err
	}
	c.mu.Lock()
	c.reqSeq++
	id := fmt.Sprintf("req-%d", c.reqSeq)
	tokens := make(chan *tunnelv1.TokenChunk, 64)
	acks := make(chan *tunnelv1.DispatchAck, 1)
	c.pending[id] = tokens
	c.acks[id] = acks
	c.mu.Unlock()

	params := opts.Params
	if params == nil {
		params = &typesv1.GenerationParams{Seed: 1, MaxTokens: 32}
	}
	d := &tunnelv1.DispatchRequest{
		RequestId:      id,
		ModelId:        opts.ModelID,
		Kind:           opts.Kind,
		Params:         params,
		Messages:       opts.Messages,
		Prompt:         opts.Prompt,
		EmbeddingInput: opts.Input,
	}
	if !opts.Deadline.IsZero() {
		d.Deadline = timestamppb.New(opts.Deadline)
	}
	if err := tunnel.SignDispatch(c.signPriv, d); err != nil {
		return "", nil, nil, err
	}
	if opts.CorruptSignature {
		d.Signature[0] ^= 0xff
	}
	if err := sess.send(&tunnelv1.CoordinatorMessage{Msg: &tunnelv1.CoordinatorMessage_Dispatch{Dispatch: d}}); err != nil {
		return "", nil, nil, err
	}
	return id, acks, tokens, nil
}

// DispatchEmbedding drives an embedding request.
func (c *Coordinator) DispatchEmbedding(modelID string, input []string) (<-chan *tunnelv1.EmbeddingResult, error) {
	sess, err := c.sessionOrErr()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.reqSeq++
	id := fmt.Sprintf("req-%d", c.reqSeq)
	ch := make(chan *tunnelv1.EmbeddingResult, 1)
	c.pendingEmb[id] = ch
	c.acks[id] = make(chan *tunnelv1.DispatchAck, 1)
	c.mu.Unlock()

	d := &tunnelv1.DispatchRequest{
		RequestId:      id,
		ModelId:        modelID,
		Kind:           typesv1.RequestKind_REQUEST_KIND_EMBEDDING,
		Params:         &typesv1.GenerationParams{Seed: 1},
		EmbeddingInput: input,
	}
	if err := tunnel.SignDispatch(c.signPriv, d); err != nil {
		return nil, err
	}
	if err := sess.send(&tunnelv1.CoordinatorMessage{Msg: &tunnelv1.CoordinatorMessage_Dispatch{Dispatch: d}}); err != nil {
		return nil, err
	}
	return ch, nil
}

// Cancel pushes a CancelRequest for an in-flight dispatch.
func (c *Coordinator) Cancel(requestID, reason string) error {
	sess, err := c.sessionOrErr()
	if err != nil {
		return err
	}
	return sess.send(&tunnelv1.CoordinatorMessage{Msg: &tunnelv1.CoordinatorMessage_Cancel{
		Cancel: &tunnelv1.CancelRequest{RequestId: requestID, Reason: reason},
	}})
}

// Challenge sends a fingerprint probe and waits for the response.
func (c *Coordinator) Challenge(ctx context.Context, modelID, prompt string) (*tunnelv1.ChallengeResponse, error) {
	sess, err := c.sessionOrErr()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.reqSeq++
	id := fmt.Sprintf("chal-%d", c.reqSeq)
	ch := make(chan *tunnelv1.ChallengeResponse, 1)
	c.pendingCh[id] = ch
	c.mu.Unlock()

	err = sess.send(&tunnelv1.CoordinatorMessage{Msg: &tunnelv1.CoordinatorMessage_Challenge{
		Challenge: &tunnelv1.Challenge{
			ChallengeId: id,
			ModelId:     modelID,
			Prompt:      prompt,
			Params:      &typesv1.GenerationParams{Seed: 12345, Temperature: 0, MaxTokens: 32},
		},
	}})
	if err != nil {
		return nil, err
	}
	select {
	case resp := <-ch:
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// AssignModels pushes a ModelAssignment.
func (c *Coordinator) AssignModels(assign []*typesv1.ModelSpec, evict []string) error {
	sess, err := c.sessionOrErr()
	if err != nil {
		return err
	}
	return sess.send(&tunnelv1.CoordinatorMessage{Msg: &tunnelv1.CoordinatorMessage_ModelAssignment{
		ModelAssignment: &tunnelv1.ModelAssignment{Assign: assign, EvictModelIds: evict},
	}})
}

// Drain asks the node to stop accepting new work.
func (c *Coordinator) Drain(reason string) error {
	sess, err := c.sessionOrErr()
	if err != nil {
		return err
	}
	return sess.send(&tunnelv1.CoordinatorMessage{Msg: &tunnelv1.CoordinatorMessage_Drain{
		Drain: &tunnelv1.Drain{Reason: reason},
	}})
}

// KickSession forcefully closes the current session (reconnect tests). The
// listener stays up so the node can re-establish.
func (c *Coordinator) KickSession() {
	c.mu.Lock()
	sess := c.session
	c.session = nil
	c.mu.Unlock()
	if sess != nil {
		close(sess.kick)
	}
}

// ---- fake CA ----

func newCA() (*x509.Certificate, ed25519.PrivateKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("fakecoord: generate CA key: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Teraflock Fake Coordinator CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * 365 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return nil, nil, fmt.Errorf("fakecoord: create CA cert: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	return cert, priv, nil
}

func (c *Coordinator) signCSR(csrPEM []byte, nodeID string) ([]byte, time.Time, error) {
	block, _ := pem.Decode(csrPEM)
	if block == nil {
		return nil, time.Time{}, fmt.Errorf("fakecoord: invalid CSR PEM")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("fakecoord: parse CSR: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, time.Time{}, fmt.Errorf("fakecoord: CSR signature: %w", err)
	}
	expires := time.Now().Add(30 * 24 * time.Hour) // 30-day rotation (SPEC §6)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: nodeID},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     expires,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.caCert, csr.PublicKey, c.caKey)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("fakecoord: sign client cert: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), expires, nil
}
