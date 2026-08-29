package tunnel

import (
	"context"
	"crypto/tls"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

// Keepalive for the coordinator connection.
//
// The daemon holds one long-lived stream open across whatever network the
// operator happens to be on, where a connection can die with neither end
// told. The heartbeat we send every few seconds proves this node is alive;
// it does not prove the path still works. On a half-open connection the
// heartbeats vanish, the coordinator closes the session and stops routing
// to us, and we notice only when the OS finally gives up on a write —
// minutes later, during which the dashboard cheerfully reports "serving".
// Pings turn that into seconds.
//
// PingInterval is a CROSS-REPO CONTRACT: the coordinator rejects clients
// that ping more often than coordinator.PingMinTime by answering GOAWAY
// "too_many_pings" and closing the connection. Pinging faster than the
// server permits does not just fail to help, it actively disconnects every
// node. Keep this >= the coordinator's PingMinTime, and never lower it
// without deploying the server first.
const (
	// PingInterval is how long the connection may sit quiet before we ping.
	PingInterval = 20 * time.Second
	// PingTimeout is how long we wait for a ping ack before concluding the
	// connection is dead and reconnecting.
	PingTimeout = 10 * time.Second
)

// keepaliveOption is applied to every coordinator dial.
func keepaliveOption() grpc.DialOption {
	return grpc.WithKeepaliveParams(keepalive.ClientParameters{
		Time:    PingInterval,
		Timeout: PingTimeout,
		// The tunnel session is a stream, but Enroll is a unary call on a
		// fresh connection; permitting pings without a stream keeps that
		// leg detectable too.
		PermitWithoutStream: true,
	})
}

// Dialer abstracts the transport so it can swap without touching the
// session logic. Production transport is QUIC via quic-go (SPEC §6);
// gRPC-over-HTTP/2 with mTLS is the current implementation and remains the
// documented fallback. TODO(quic): add a quic-go Dialer implementing this
// interface once the coordinator terminates QUIC.
type Dialer interface {
	Dial(ctx context.Context, addr string) (*grpc.ClientConn, error)
}

// TLSDialer dials gRPC over TLS/H2, optionally with a client certificate
// (mTLS after enrollment).
type TLSDialer struct {
	// TLS may carry RootCAs (pinned coordinator CA) and Certificates
	// (client cert from enrollment).
	TLS *tls.Config
}

func (d *TLSDialer) Dial(_ context.Context, addr string) (*grpc.ClientConn, error) {
	cfg := d.TLS
	if cfg == nil {
		cfg = &tls.Config{MinVersion: tls.VersionTLS13}
	}
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(credentials.NewTLS(cfg)),
		keepaliveOption(),
	)
	if err != nil {
		return nil, fmt.Errorf("tunnel: dial %s: %w", addr, err)
	}
	return conn, nil
}

// InsecureDialer is for local development only.
type InsecureDialer struct{}

func (InsecureDialer) Dial(_ context.Context, addr string) (*grpc.ClientConn, error) {
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		keepaliveOption(),
	)
	if err != nil {
		return nil, fmt.Errorf("tunnel: dial %s: %w", addr, err)
	}
	return conn, nil
}
