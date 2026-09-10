package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/teraflock/flockd/internal/config"
	"github.com/teraflock/flockd/internal/enroll"
	"github.com/teraflock/flockd/internal/tunnel"
)

// bootstrap TLS against a coordinator whose server cert is issued by its
// own mesh CA (flockd#4): the real handshake, not just pool membership.

type testCA struct {
	cert *x509.Certificate
	key  ed25519.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T, name string) *testCA {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: cert, key: priv, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// serverCert issues a loopback server keypair from the CA.
func (ca *testCA) serverCert(t *testing.T) tls.Certificate {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "coordinator"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, pub, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}
}

// serveTLS listens on loopback with cert, completing (or failing) one
// handshake per connection, and returns the address.
func serveTLS(t *testing.T, cert tls.Certificate) string {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_ = c.(*tls.Conn).Handshake()
				_ = c.Close()
			}()
		}
	}()
	return ln.Addr().String()
}

func TestBootstrapTLSTrustsPinnedAndEnrolledMeshCA(t *testing.T) {
	mesh := newTestCA(t, "mesh CA")
	other := newTestCA(t, "other mesh CA")
	addr := serveTLS(t, mesh.serverCert(t))

	handshake := func(cfg config.Config, creds *enroll.Credentials) error {
		tlsCfg, err := bootstrapTLSConfig(cfg, creds)
		if err != nil {
			return err
		}
		d := &net.Dialer{Timeout: 5 * time.Second}
		conn, err := tls.DialWithDialer(d, "tcp", addr, tlsCfg)
		if err == nil {
			_ = conn.Close()
		}
		return err
	}
	base := config.Default()

	// The bug: system roots alone reject the mesh-CA-issued server cert.
	if err := handshake(base, nil); err == nil {
		t.Fatal("system roots alone accepted a mesh-CA server cert")
	}
	// Enrollment against a self-hosted coordinator: tunnel.ca_cert, inline
	// or as a file.
	pinned := base
	pinned.Tunnel.CACert = string(mesh.pem)
	if err := handshake(pinned, nil); err != nil {
		t.Fatalf("inline pinned CA: %v", err)
	}
	caPath := filepath.Join(t.TempDir(), "mesh-ca.pem")
	if err := os.WriteFile(caPath, mesh.pem, 0o600); err != nil {
		t.Fatal(err)
	}
	pinned.Tunnel.CACert = caPath
	if err := handshake(pinned, nil); err != nil {
		t.Fatalf("pinned CA file: %v", err)
	}
	// Rotation: the CA the node enrolled with is enough on its own.
	if err := handshake(base, &enroll.Credentials{CACertPEM: mesh.pem}); err != nil {
		t.Fatalf("enrolled mesh CA on the rotation dial: %v", err)
	}
	// A genuinely untrusted cert still fails: a different CA pinned and a
	// different mesh's credentials do not vouch for this server.
	untrusted := base
	untrusted.Tunnel.CACert = string(other.pem)
	if err := handshake(untrusted, &enroll.Credentials{CACertPEM: other.pem}); err == nil {
		t.Fatal("server cert from an unrelated CA accepted")
	}
	// A pinned CA that does not parse is an error, not a silent skip.
	bad := base
	bad.Tunnel.CACert = "-----BEGIN CERTIFICATE-----\nbm9wZQ==\n-----END CERTIFICATE-----\n"
	if _, err := bootstrapTLSConfig(bad, nil); err == nil {
		t.Fatal("garbage tunnel.ca_cert accepted")
	}
	if _, err := bootstrapDialer(bad, nil); err == nil {
		t.Fatal("bootstrapDialer built a dialer from a garbage tunnel.ca_cert")
	}

	// Dev plaintext keeps bypassing TLS entirely.
	dev := base
	dev.Tunnel.Insecure = true
	d, err := bootstrapDialer(dev, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := d.(tunnel.InsecureDialer); !ok {
		t.Fatalf("insecure dialer = %T", d)
	}
}
