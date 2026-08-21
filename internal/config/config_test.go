package config

import (
	"os"
	"path/filepath"
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
