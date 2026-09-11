package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultsValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
}

func TestLoadTOMLAndEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	tomlBody := `
data_dir = "/tmp/flocktest"

[governor]
serve_policy = "always"
yield_grace = "5s"

[local_api]
listen = "127.0.0.1:8811"

[models]
pin = ["llama-3.1-8b-instruct-q4_k_m"]
`
	if err := os.WriteFile(path, []byte(tomlBody), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLOCKD_LOCAL_API__LISTEN", "127.0.0.1:9999")
	t.Setenv("FLOCKD_RUNTIME__KIND", "mock")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != "/tmp/flocktest" {
		t.Errorf("data_dir = %q", cfg.DataDir)
	}
	if cfg.Governor.ServePolicy != "always" {
		t.Errorf("serve_policy = %q", cfg.Governor.ServePolicy)
	}
	if cfg.Governor.YieldGrace != 5*time.Second {
		t.Errorf("yield_grace = %v", cfg.Governor.YieldGrace)
	}
	// env overrides file
	if cfg.LocalAPI.Listen != "127.0.0.1:9999" {
		t.Errorf("listen = %q", cfg.LocalAPI.Listen)
	}
	if cfg.Runtime.Kind != "mock" {
		t.Errorf("runtime.kind = %q", cfg.Runtime.Kind)
	}
	if len(cfg.Models.Pin) != 1 {
		t.Errorf("pin = %v", cfg.Models.Pin)
	}
	// untouched default survives
	if cfg.Budget.MaxVRAMPercent != 80 {
		t.Errorf("max_vram_percent = %d", cfg.Budget.MaxVRAMPercent)
	}
}

func TestValidateRejectsBadPolicy(t *testing.T) {
	c := Default()
	c.Governor.ServePolicy = "sometimes"
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for bad serve_policy")
	}
}

func TestLimitsOverlayRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	// dir contains raw backslashes on Windows (t.TempDir()); %q escapes them
	// the way TOML basic strings require, unlike naive quote-wrapping.
	data := fmt.Sprintf("data_dir = %q\n[governor]\nserve_policy = \"idle-only\"\n", dir)
	if err := os.WriteFile(cfgPath, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	// Without an overlay, config.toml wins.
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Governor.ServePolicy != "idle-only" {
		t.Fatalf("serve_policy = %q", cfg.Governor.ServePolicy)
	}

	// SaveLimits then reload: the overlay wins.
	if err := SaveLimits(dir, Governor{
		ServePolicy:    "always",
		IdleAfter:      90 * time.Second,
		YieldGrace:     3 * time.Second,
		ServeOnBattery: true,
		MaxTempCelsius: 85,
		Schedule:       []string{"22:00-08:00"},
	}, LiveLimits{MeshManaged: false, MaxDiskMB: 12345, RetentionDays: 14, IdleUnloadS: 600, MaxRAMMB: 8192}); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	g := cfg.Governor
	if g.ServePolicy != "always" || g.IdleAfter != 90*time.Second || g.YieldGrace != 3*time.Second ||
		!g.ServeOnBattery || g.MaxTempCelsius != 85 || len(g.Schedule) != 1 || g.Schedule[0] != "22:00-08:00" {
		t.Fatalf("overlay not applied: %+v", g)
	}
	// The mesh_managed switch rides the same overlay (default is on).
	if cfg.Models.MeshManaged {
		t.Fatal("overlay mesh_managed=false not applied")
	}
	if cfg.Models.MaxDiskMB != 12345 || cfg.Models.RetentionDays != 14 || cfg.Models.IdleUnloadS != 600 || cfg.Budget.MaxRAMMB != 8192 {
		t.Fatalf("overlay model/budget limits not applied: %+v %+v", cfg.Models, cfg.Budget)
	}
}

func TestRuntimeSigningKeys(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "runtime-signing.pub")
	const pemKey = "-----BEGIN PUBLIC KEY-----\nMFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE\n-----END PUBLIC KEY-----\n"
	if err := os.WriteFile(keyFile, []byte(pemKey), 0o600); err != nil {
		t.Fatal(err)
	}

	// Defaults: no pin override (the embedded key is the pin), signatures
	// required (flockd#25 stage 2).
	def := Default()
	if def.Runtime.ArtifactSigningKey != "" || !def.Runtime.RequireSignature {
		t.Fatalf("defaults: key=%q require=%v", def.Runtime.ArtifactSigningKey, def.Runtime.RequireSignature)
	}
	if b, err := def.Runtime.ArtifactSigningKeyPEM(); err != nil || b != nil {
		t.Fatalf("unset key resolves to (%q, %v), want (nil, nil)", b, err)
	}

	// TOML: path form.
	path := filepath.Join(dir, "config.toml")
	body := fmt.Sprintf("[runtime]\nartifact_signing_key = %q\nrequire_signature = true\n", keyFile)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Runtime.RequireSignature {
		t.Error("require_signature not loaded")
	}
	if b, err := cfg.Runtime.ArtifactSigningKeyPEM(); err != nil || string(b) != pemKey {
		t.Errorf("path form resolved to (%q, %v)", b, err)
	}

	// Env: inline PEM form (double-underscore section separator), and it
	// overrides the file.
	t.Setenv("FLOCKD_RUNTIME__ARTIFACT_SIGNING_KEY", pemKey)
	t.Setenv("FLOCKD_RUNTIME__REQUIRE_SIGNATURE", "false")
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runtime.RequireSignature {
		t.Error("env must override require_signature")
	}
	if b, err := cfg.Runtime.ArtifactSigningKeyPEM(); err != nil || !strings.HasPrefix(string(b), "-----BEGIN PUBLIC KEY-----") || !strings.HasSuffix(string(b), "-----END PUBLIC KEY-----\n") {
		t.Errorf("inline form resolved to (%q, %v)", b, err)
	}

	// A missing file is a load-time error, not a silent "no pin".
	cfg.Runtime.ArtifactSigningKey = filepath.Join(dir, "missing.pub")
	if _, err := cfg.Runtime.ArtifactSigningKeyPEM(); err == nil || !strings.Contains(err.Error(), "artifact_signing_key") {
		t.Errorf("missing key file: want error naming the key, got %v", err)
	}
}

// testCAPEM is a self-signed CA certificate for the tunnel.ca_cert tests.
func testCAPEM(t *testing.T) []byte {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test mesh CA"},
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
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestTunnelCACert(t *testing.T) {
	caPEM := testCAPEM(t)
	dir := t.TempDir()
	caPath := filepath.Join(dir, "mesh-ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	c := Default()
	if got, err := c.Tunnel.CACertPEM(); err != nil || got != nil {
		t.Fatalf("unset: %q, %v; want nil, nil", got, err)
	}
	// A file path resolves to the file's contents.
	c.Tunnel.CACert = caPath
	if got, err := c.Tunnel.CACertPEM(); err != nil || string(got) != string(caPEM) {
		t.Fatalf("path: %q, %v", got, err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate with a CA file: %v", err)
	}
	// Inline PEM is used as-is.
	c.Tunnel.CACert = "\n" + string(caPEM) + "\n"
	if got, err := c.Tunnel.CACertPEM(); err != nil || string(got) != strings.TrimSpace(string(caPEM)) {
		t.Fatalf("inline: %q, %v", got, err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate with inline CA: %v", err)
	}
	// A pinned CA that cannot be used fails validation rather than being
	// ignored — the setting exists for a coordinator system roots reject.
	c.Tunnel.CACert = filepath.Join(dir, "missing.pem")
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "tunnel.ca_cert") {
		t.Fatalf("missing file: %v", err)
	}
	garbage := filepath.Join(dir, "garbage.pem")
	if err := os.WriteFile(garbage, []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.Tunnel.CACert = garbage
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "tunnel.ca_cert") {
		t.Fatalf("garbage file: %v", err)
	}
	c.Tunnel.CACert = "-----BEGIN CERTIFICATE-----\nbm9wZQ==\n-----END CERTIFICATE-----\n"
	if err := c.Validate(); err == nil {
		t.Fatal("inline garbage accepted")
	}

	// Round trip through the file and the environment.
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[tunnel]\nca_cert = \""+filepath.ToSlash(caPath)+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.ToSlash(cfg.Tunnel.CACert) != filepath.ToSlash(caPath) {
		t.Errorf("ca_cert from toml = %q, want %q", cfg.Tunnel.CACert, caPath)
	}
	t.Setenv("FLOCKD_TUNNEL__CA_CERT", string(caPEM))
	cfg, err = Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Tunnel.CACert != string(caPEM) {
		t.Errorf("env did not override ca_cert: %q", cfg.Tunnel.CACert)
	}
	t.Setenv("FLOCKD_TUNNEL__CA_CERT", filepath.Join(dir, "missing.pem"))
	if _, err := Load(cfgPath); err == nil {
		t.Fatal("Load accepted an unreadable tunnel.ca_cert")
	}
}
