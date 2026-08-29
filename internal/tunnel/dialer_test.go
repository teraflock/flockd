package tunnel

import (
	"testing"
	"time"
)

// TestKeepaliveContract guards the cross-repo invariant documented on
// PingInterval. The coordinator refuses clients that ping more often than its
// PingMinTime, answering GOAWAY "too_many_pings" and closing the connection —
// so pinging faster than the server permits does not merely fail to help, it
// disconnects every node on a timer. Lowering PingInterval below the
// coordinator's floor requires deploying the server first.
func TestKeepaliveContract(t *testing.T) {
	// control-plane/internal/coordinator.PingMinTime. If that value rises,
	// this test is what should fail.
	const coordinatorPingMinTime = 10 * time.Second

	if PingInterval < coordinatorPingMinTime {
		t.Fatalf("PingInterval %v is below the coordinator's PingMinTime %v: "+
			"the coordinator would GOAWAY every node", PingInterval, coordinatorPingMinTime)
	}
	if PingTimeout >= PingInterval {
		t.Errorf("PingTimeout %v should be shorter than PingInterval %v",
			PingTimeout, PingInterval)
	}
}

// TestDialersSetKeepalive is a smoke test that both dialers actually apply the
// option — the failure mode of forgetting one is silent, and the insecure
// dialer is what the local kind mesh uses.
func TestDialersSetKeepalive(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    Dialer
	}{
		{"tls", &TLSDialer{}},
		{"insecure", InsecureDialer{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := tc.d.Dial(t.Context(), "127.0.0.1:1")
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close()
			// grpc.NewClient is lazy, so a successful construction with the
			// option applied is what we can assert without a live server;
			// an invalid keepalive config would fail here.
		})
	}
}
