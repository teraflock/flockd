package enroll

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
)

// ClientTLSConfig builds the mTLS client config for the coordinator tunnel
// from the node identity and enrollment credentials. The client certificate
// is the one the mesh CA issued at enrollment; the server is verified
// against system roots (production presents a public certificate for
// tunnel.teraflock.ai) with the mesh CA appended for self-hosted and test
// coordinators that serve a mesh-CA-issued cert.
func ClientTLSConfig(id *Identity, creds *Credentials, insecureSkipVerify bool) (*tls.Config, error) {
	keyDER, err := x509.MarshalPKCS8PrivateKey(id.Priv)
	if err != nil {
		return nil, fmt.Errorf("enroll: marshal key for tls: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(creds.ClientCertPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("enroll: client keypair: %w", err)
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}
	if !roots.AppendCertsFromPEM(creds.CACertPEM) {
		return nil, fmt.Errorf("enroll: coordinator CA cert unparsable")
	}
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{cert},
		RootCAs:            roots,
		InsecureSkipVerify: insecureSkipVerify, //nolint:gosec // dev flag, documented
	}, nil
}
