package tunnel

import (
	"context"
	"crypto/tls"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

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
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
	if err != nil {
		return nil, fmt.Errorf("tunnel: dial %s: %w", addr, err)
	}
	return conn, nil
}

// InsecureDialer is for local development only.
type InsecureDialer struct{}

func (InsecureDialer) Dial(_ context.Context, addr string) (*grpc.ClientConn, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("tunnel: dial %s: %w", addr, err)
	}
	return conn, nil
}
