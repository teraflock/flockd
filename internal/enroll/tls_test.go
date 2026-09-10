package enroll

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"
)

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

// issue signs a leaf for pub: a server cert for dnsName when set, else a
// client cert.
func (ca *testCA) issue(t *testing.T, pub ed25519.PublicKey, dnsName string) (*x509.Certificate, []byte) {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "leaf"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	if dnsName != "" {
		tmpl.DNSNames = []string{dnsName}
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, pub, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func verifies(leaf *x509.Certificate, roots *x509.CertPool) bool {
	_, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "coordinator.test"})
	return err == nil
}

func TestRootPool(t *testing.T) {
	mesh := newTestCA(t, "mesh CA")
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	server, _ := mesh.issue(t, pub, "coordinator.test")

	// System roots alone reject a mesh-CA-issued server cert — the
	// self-hosted coordinator case (flockd#4).
	system, err := RootPool()
	if err != nil {
		t.Fatal(err)
	}
	if verifies(server, system) {
		t.Fatal("system roots verified a private-CA leaf")
	}
	// Appending the mesh CA (from enrollment, or tunnel.ca_cert) fixes it;
	// empty extras are skipped rather than rejected.
	pool, err := RootPool(nil, []byte{}, mesh.pem)
	if err != nil {
		t.Fatal(err)
	}
	if !verifies(server, pool) {
		t.Fatal("pool with the mesh CA did not verify its leaf")
	}
	// A different private CA does not: no blanket trust.
	other := newTestCA(t, "other CA")
	pool, err = RootPool(other.pem)
	if err != nil {
		t.Fatal(err)
	}
	if verifies(server, pool) {
		t.Fatal("pool with an unrelated CA verified the leaf")
	}
	// Garbage is an error, never a silently narrower pool.
	if _, err := RootPool(mesh.pem, []byte("-----BEGIN CERTIFICATE-----\nbm9wZQ==\n-----END CERTIFICATE-----\n")); err == nil {
		t.Fatal("unparsable PEM accepted")
	}
	if _, err := RootPool([]byte("not pem at all")); err == nil {
		t.Fatal("non-PEM accepted")
	}
}

func TestClientTLSConfigTrustsEnrolledAndPinnedCA(t *testing.T) {
	mesh := newTestCA(t, "mesh CA")
	private := newTestCA(t, "corporate CA")
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := &Identity{Priv: priv, Pub: pub}
	_, clientPEM := mesh.issue(t, pub, "")
	creds := &Credentials{NodeID: "n1", ClientCertPEM: clientPEM, CACertPEM: mesh.pem}

	srvPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	meshServer, _ := mesh.issue(t, srvPub, "coordinator.test")
	privateServer, _ := private.issue(t, srvPub, "coordinator.test")

	// Without a pinned CA: the enrolled mesh CA is trusted (self-hosted
	// coordinator serving a mesh-CA cert), a private CA's cert is not.
	cfg, err := ClientTLSConfig(id, creds, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Certificates) != 1 || cfg.MinVersion != 0x0304 {
		t.Fatalf("config = %+v", cfg)
	}
	if !verifies(meshServer, cfg.RootCAs) {
		t.Fatal("enrolled mesh CA not trusted")
	}
	if verifies(privateServer, cfg.RootCAs) {
		t.Fatal("unpinned private CA trusted")
	}
	// tunnel.ca_cert adds the private CA for the session too.
	cfg, err = ClientTLSConfig(id, creds, private.pem, false)
	if err != nil {
		t.Fatal(err)
	}
	if !verifies(meshServer, cfg.RootCAs) || !verifies(privateServer, cfg.RootCAs) {
		t.Fatal("pinned CA did not join the enrolled CA in the pool")
	}
	// Broken inputs are errors, not silent fallbacks.
	if _, err := ClientTLSConfig(id, creds, []byte("garbage"), false); err == nil || !strings.Contains(err.Error(), "tunnel.ca_cert") {
		t.Fatalf("garbage pinned CA: %v", err)
	}
	noCA := *creds
	noCA.CACertPEM = nil
	if _, err := ClientTLSConfig(id, &noCA, nil, false); err == nil {
		t.Fatal("credentials without a CA accepted")
	}
}
