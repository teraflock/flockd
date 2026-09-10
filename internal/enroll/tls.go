package enroll

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
)

// RootPool is the server-verification pool for every coordinator dial:
// the system roots (production presents a public certificate for
// tunnel.teraflock.ai) plus each non-empty extra PEM — the mesh CA the
// node enrolled with, and tunnel.ca_cert when the operator pinned one for
// a self-hosted coordinator. A non-empty PEM that holds no certificate is
// an error rather than a silently narrower pool.
func RootPool(extra ...[]byte) (*x509.CertPool, error) {
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}
	for _, p := range extra {
		if len(p) == 0 {
			continue
		}
		if !roots.AppendCertsFromPEM(p) {
			return nil, fmt.Errorf("enroll: CA cert unparsable")
		}
	}
	return roots, nil
}

// ClientTLSConfig builds the mTLS client config for the coordinator tunnel
// from the node identity and enrollment credentials. The client certificate
// is the one the mesh CA issued at enrollment; the server is verified
// against RootPool with the enrolled mesh CA and the operator's pinned CA
// (tunnel.ca_cert, may be nil) appended for self-hosted and test
// coordinators that serve a private-CA-issued cert.
func ClientTLSConfig(id *Identity, creds *Credentials, pinnedCA []byte, insecureSkipVerify bool) (*tls.Config, error) {
	keyDER, err := x509.MarshalPKCS8PrivateKey(id.Priv)
	if err != nil {
		return nil, fmt.Errorf("enroll: marshal key for tls: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(creds.ClientCertPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("enroll: client keypair: %w", err)
	}
	if len(creds.CACertPEM) == 0 {
		return nil, fmt.Errorf("enroll: credentials carry no coordinator CA cert")
	}
	roots, err := RootPool(creds.CACertPEM, pinnedCA)
	if err != nil {
		return nil, fmt.Errorf("%w (coordinator CA from enrollment, or tunnel.ca_cert)", err)
	}
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{cert},
		RootCAs:            roots,
		InsecureSkipVerify: insecureSkipVerify, //nolint:gosec // dev flag, documented
	}, nil
}
