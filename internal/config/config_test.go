package config

import (
	"fmt"
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

	// Defaults: no pin override, signatures not (yet) required.
	def := Default()
	if def.Runtime.ArtifactSigningKey != "" || def.Runtime.RequireSignature {
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
