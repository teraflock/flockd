// Package enroll manages node identity (Ed25519 keypair that never leaves
// the device), CSR generation, the Enroll RPC, credential storage, and the
// `hive login` PKCE-style browser handoff (SPEC §4.1, §6).
package enroll

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"crypto/x509/pkix"

	tunnelv1 "github.com/hivegrid/proto/gen/go/hive/tunnel/v1"
	typesv1 "github.com/hivegrid/proto/gen/go/hive/types/v1"
)

const (
	keyFile   = "node.key"       // PKCS8 Ed25519 private key, 0600
	credsFile = "node_creds.pem" // client cert + CA + coordinator pubkey
)

// Identity is the node's Ed25519 keypair. NodeID = key fingerprint.
type Identity struct {
	Priv ed25519.PrivateKey
	Pub  ed25519.PublicKey
}

// Fingerprint is the node identity string (sha256 of the raw public key).
func (id *Identity) Fingerprint() string {
	sum := sha256.Sum256(id.Pub)
	return hex.EncodeToString(sum[:16])
}

// LoadOrGenerateIdentity returns the persisted identity, creating one with
// 0600 permissions on first run.
//
// TODO(keychain): move private key storage to OS keychain / DPAPI / secret
// service; file with 0600 is the documented Phase 0 baseline.
func LoadOrGenerateIdentity(dataDir string) (*Identity, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("enroll: mkdir data dir: %w", err)
	}
	path := filepath.Join(dataDir, keyFile)
	raw, err := os.ReadFile(path)
	if err == nil {
		block, _ := pem.Decode(raw)
		if block == nil {
			return nil, fmt.Errorf("enroll: %s is not PEM", path)
		}
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("enroll: parse node key: %w", err)
		}
		priv, ok := k.(ed25519.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("enroll: node key is not Ed25519")
		}
		return &Identity{Priv: priv, Pub: priv.Public().(ed25519.PublicKey)}, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("enroll: read node key: %w", err)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("enroll: generate keypair: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("enroll: marshal key: %w", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		return nil, fmt.Errorf("enroll: write node key: %w", err)
	}
	return &Identity{Priv: priv, Pub: pub}, nil
}

// CSR generates a PEM-encoded certificate signing request for the identity.
func (id *Identity) CSR() ([]byte, error) {
	tmpl := &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: id.Fingerprint()},
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, id.Priv)
	if err != nil {
		return nil, fmt.Errorf("enroll: create CSR: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}

// Credentials are the enrollment artifacts persisted to the data dir.
type Credentials struct {
	NodeID            string
	ClientCertPEM     []byte
	CACertPEM         []byte
	CoordinatorPubKey ed25519.PublicKey
	CertExpiresAt     time.Time
}

// Enroll performs the Enroll RPC with a claim code and persists the result.
func Enroll(ctx context.Context, client tunnelv1.TunnelServiceClient, id *Identity, claimCode string, cap *typesv1.CapabilityProfile, dataDir string) (*Credentials, error) {
	csr, err := id.CSR()
	if err != nil {
		return nil, err
	}
	resp, err := client.Enroll(ctx, &tunnelv1.EnrollRequest{
		ClaimCode:  claimCode,
		Pubkey:     id.Pub,
		CsrPem:     csr,
		Capability: cap,
	})
	if err != nil {
		return nil, fmt.Errorf("enroll: rpc: %w", err)
	}
	creds := &Credentials{
		NodeID:            resp.GetNodeId(),
		ClientCertPEM:     resp.GetClientCertPem(),
		CACertPEM:         resp.GetCaCertPem(),
		CoordinatorPubKey: ed25519.PublicKey(resp.GetCoordinatorPubkey()),
		CertExpiresAt:     resp.GetCertExpiresAt().AsTime(),
	}
	if len(creds.CoordinatorPubKey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("enroll: coordinator returned invalid signing key")
	}
	if err := SaveCredentials(dataDir, creds); err != nil {
		return nil, err
	}
	return creds, nil
}

// RotateIfNeeded re-enrolls when the client cert is within 7 days of
// expiry. Cert rotation happens over the tunnel every 30 days (SPEC §6).
// TODO(rotation): the real coordinator will accept rotation without a claim
// code, authenticated by the existing mTLS session; the fake accepts the
// sentinel code below.
func RotateIfNeeded(ctx context.Context, client tunnelv1.TunnelServiceClient, id *Identity, creds *Credentials, cap *typesv1.CapabilityProfile, dataDir string) (*Credentials, error) {
	if time.Until(creds.CertExpiresAt) > 7*24*time.Hour {
		return creds, nil
	}
	return Enroll(ctx, client, id, "rotate:"+creds.NodeID, cap, dataDir)
}

// SaveCredentials persists creds with 0600.
func SaveCredentials(dataDir string, c *Credentials) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("enroll: mkdir credentials dir: %w", err)
	}
	var buf []byte
	buf = append(buf, pem.EncodeToMemory(&pem.Block{
		Type:  "HIVEGRID NODE ID",
		Bytes: []byte(c.NodeID),
	})...)
	buf = append(buf, c.ClientCertPEM...)
	buf = append(buf, c.CACertPEM...)
	buf = append(buf, pem.EncodeToMemory(&pem.Block{
		Type:    "HIVEGRID COORDINATOR PUBKEY",
		Bytes:   c.CoordinatorPubKey,
		Headers: map[string]string{"Expires": c.CertExpiresAt.UTC().Format(time.RFC3339)},
	})...)
	path := filepath.Join(dataDir, credsFile)
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		return fmt.Errorf("enroll: write credentials: %w", err)
	}
	return nil
}

// LoadCredentials reads persisted enrollment artifacts; os.ErrNotExist if
// the node has never enrolled.
func LoadCredentials(dataDir string) (*Credentials, error) {
	raw, err := os.ReadFile(filepath.Join(dataDir, credsFile))
	if err != nil {
		return nil, err
	}
	c := &Credentials{}
	rest := raw
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		switch block.Type {
		case "HIVEGRID NODE ID":
			c.NodeID = string(block.Bytes)
		case "CERTIFICATE":
			// First cert = client, second = CA.
			p := pem.EncodeToMemory(block)
			if c.ClientCertPEM == nil {
				c.ClientCertPEM = p
			} else {
				c.CACertPEM = p
			}
		case "HIVEGRID COORDINATOR PUBKEY":
			c.CoordinatorPubKey = ed25519.PublicKey(block.Bytes)
			if exp := block.Headers["Expires"]; exp != "" {
				c.CertExpiresAt, _ = time.Parse(time.RFC3339, exp)
			}
		}
	}
	if c.NodeID == "" || len(c.CoordinatorPubKey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("enroll: credentials file corrupt")
	}
	return c, nil
}

// Enrolled reports whether credentials exist.
func Enrolled(dataDir string) bool {
	_, err := os.Stat(filepath.Join(dataDir, credsFile))
	return err == nil
}
